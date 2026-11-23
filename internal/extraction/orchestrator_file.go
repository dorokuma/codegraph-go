package extraction

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/resolution"
)

// fileNodeCount returns the stored node_count for store, honoring the
// nodeCountFn test seam.
func (o *Orchestrator) fileNodeCount(store string) (int, error) {
	if o.nodeCountFn != nil {
		return o.nodeCountFn(store)
	}
	return o.db.GetFileNodeCount(store)
}

// splitNameLineKey parses keys produced as fmt.Sprintf("%s:%d", name, line).

// IndexFile indexes a single file.
func (o *Orchestrator) IndexFile(path string) (int, error) {
	lang := DetectLanguage(path)
	if lang == "" || !IsSupportedLanguage(lang) {
		return 0, fmt.Errorf("unsupported language for %s", path)
	}
	n, err := o.indexFile(path, lang, nil)
	if err != nil {
		return n, err
	}
	key := o.storePath(path)
	var join error
	if _, rerr := resolution.ResolveForFilesContext(o.indexContext(), o.db, o.workdir, []string{key}); rerr != nil {
		log.Printf("resolve after index %s: %v", path, rerr)
		join = errors.Join(join, rerr)
	}
	if serr := o.runSynthesis([]string{key}); serr != nil {
		join = errors.Join(join, serr)
	}
	return n, join
}

// DeleteFile removes a file from the index.
// path may be absolute (watcher) or relative; both map to the storage key.
func (o *Orchestrator) DeleteFile(path string) error {
	return o.db.ClearFile(o.storePath(path))
}

// DeleteTree removes every indexed file whose storage key is path or lives
// under path/ (directory rename/delete). ListFilesInDir only matches
// direct children and cannot prune a whole tree.
func (o *Orchestrator) DeleteTree(path string) error {
	key := o.storePath(path)
	if key == "" {
		return nil
	}
	files, err := o.db.ListFilesContext(o.indexContext())
	if err != nil {
		if o.interrupted() {
			return errors.Join(err, ErrIndexInterrupted)
		}
		return err
	}
	var errs []error
	prefix := key + "/"
	for _, f := range files {
		if o.interrupted() {
			return errors.Join(append(errs, ErrIndexInterrupted)...)
		}
		if key == "." || f == key || strings.HasPrefix(f, prefix) {
			// A started ClearFile transaction completes atomically. Check the
			// shutdown signal before starting the next file's transaction.
			if cerr := o.db.ClearFile(f); cerr != nil {
				errs = append(errs, cerr)
			}
		}
	}
	if o.interrupted() {
		errs = append(errs, ErrIndexInterrupted)
	}
	return errors.Join(errs...)
}

// pruneMissingFiles drops files-table rows (and their nodes) that are no
// longer among this pass's walk jobs. Caller must not invoke this when
// the walk itself failed — an incomplete job set would wipe the index.
// Returns how many rows it removed: files-table rows via ClearFile plus
// every row the bad-key tail cleanups deleted. IndexAll's zero-change
// short-circuit needs this count — a prune that dropped nodes changed the
// index, so resolve+synthesis must still run even when no file indexed.
func (o *Orchestrator) pruneMissingFiles(jobs []indexJob) (int, error) {
	keep := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		if o.interrupted() {
			return 0, ErrIndexInterrupted
		}
		keep[o.storePath(j.path)] = struct{}{}
	}
	listed, err := o.db.ListFilesContext(o.indexContext())
	if err != nil {
		if o.interrupted() {
			return 0, errors.Join(err, ErrIndexInterrupted)
		}
		return 0, err
	}
	var errs []error
	var pruned int
	for _, f := range listed {
		if o.interrupted() {
			return pruned, errors.Join(append(errs, ErrIndexInterrupted)...)
		}
		if _, ok := keep[f]; ok {
			continue
		}
		if cerr := o.db.ClearFile(f); cerr != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", f, cerr))
		} else {
			pruned++
		}
	}
	// Targeted cleanup of ".."-shaped invalid keys in files/nodes/
	// unresolved_refs: StoragePath never produces them (its output is a
	// workdir-relative key), so these rows are orphans the keep-set pass
	// above either cannot reach (nodes/unresolved_refs rows have no files
	// entry) or must not keep (a files row whose path literally walks out
	// of the tree). "%/.." covers keys ENDING in a parent-dir segment,
	// which the earlier patterns missed. Each statement is gated on the
	// interrupt check so a shutdown still stops between transactions.
	for _, q := range []string{
		`DELETE FROM files WHERE path LIKE '../%' OR path LIKE '%/../%' OR path LIKE '%/..' OR path = '..'`,
		`DELETE FROM nodes WHERE file LIKE '../%' OR file LIKE '%/../%' OR file LIKE '%/..' OR file = '..'`,
		`DELETE FROM unresolved_refs WHERE file_path LIKE '../%' OR file_path LIKE '%/../%' OR file_path LIKE '%/..' OR file_path = '..'`,
	} {
		if o.interrupted() {
			return pruned, errors.Join(append(errs, ErrIndexInterrupted)...)
		}
		affected, derr := o.db.ExecContext(o.indexContext(), q)
		if derr != nil {
			errs = append(errs, fmt.Errorf("prune bad keys: %w", derr))
		} else {
			pruned += int(affected)
		}
	}
	if o.interrupted() {
		errs = append(errs, ErrIndexInterrupted)
	}
	return pruned, errors.Join(errs...)
}

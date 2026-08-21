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
	if _, rerr := resolution.ResolveForFiles(o.db, o.workdir, []string{key}); rerr != nil {
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

// DeleteFile removes a file from the index.
// path may be absolute (watcher) or relative; both map to the storage key.
func (o *Orchestrator) DeleteFile(path string) error {
	return o.db.ClearFile(o.storePath(path))
}

// DeleteTree removes every indexed file whose storage key is path or lives
// under path/ (directory rename/delete). ListFilesInDir only matches
// direct children and cannot prune a whole tree.

// DeleteTree removes every indexed file whose storage key is path or lives
// under path/ (directory rename/delete). ListFilesInDir only matches
// direct children and cannot prune a whole tree.
func (o *Orchestrator) DeleteTree(path string) error {
	key := o.storePath(path)
	if key == "" {
		return nil
	}
	files, err := o.db.ListFiles()
	if err != nil {
		return err
	}
	var errs []error
	if key == "." {
		for _, f := range files {
			if cerr := o.db.ClearFile(f); cerr != nil {
				errs = append(errs, cerr)
			}
		}
		return errors.Join(errs...)
	}
	prefix := key + "/"
	for _, f := range files {
		if f == key || strings.HasPrefix(f, prefix) {
			if cerr := o.db.ClearFile(f); cerr != nil {
				errs = append(errs, cerr)
			}
		}
	}
	return errors.Join(errs...)
}

// pruneMissingFiles drops files-table rows (and their nodes) that are no
// longer among this pass's walk jobs. Caller must not invoke this when
// the walk itself failed — an incomplete job set would wipe the index.

// pruneMissingFiles drops files-table rows (and their nodes) that are no
// longer among this pass's walk jobs. Caller must not invoke this when
// the walk itself failed — an incomplete job set would wipe the index.
func (o *Orchestrator) pruneMissingFiles(jobs []indexJob) error {
	keep := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		keep[o.storePath(j.path)] = struct{}{}
	}
	listed, err := o.db.ListFiles()
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range listed {
		if _, ok := keep[f]; ok {
			continue
		}
		if cerr := o.db.ClearFile(f); cerr != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", f, cerr))
		}
	}
	return errors.Join(errs...)
}

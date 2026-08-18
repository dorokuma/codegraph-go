package extraction

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/codegraph-go/internal/config"
	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/resolution"
)

// Orchestrator manages the extraction pipeline.
// Step 2 model: extract → write nodes/files → same-file edges now;
// cross-file refs land in unresolved_refs (pending) for step 3 resolution.
type Orchestrator struct {
	db      *db.DB
	workdir string

	// done, when set via SetDone, is polled by the index loops so a shutdown
	// (daemon Stop closes BgDone) can interrupt a long index pass promptly
	// instead of finishing the whole workspace first — the 8/6 half-dead
	// daemon stuck Stop's BgWg.Wait() behind a full IndexAll pass while the
	// process held the DB with no socket serving. nil disables the checks
	// (tests, direct mode without a shutdown channel).
	done <-chan struct{}

	// force is accessed from IndexAll worker goroutines and background
	// RebuildAll concurrently — guarded by forceMu (A9).
	forceMu sync.Mutex
	force   bool // when true, indexIfNeeded always reindexes

	// extractFn, when non-nil, replaces the built-in ts→regex extraction
	// pipeline. Test seam for parse-failure paths (A4/S1). The bool reports
	// whether tree-sitter itself errored (regex fallback took over); it lets
	// the caller distinguish "unparseable file" from "successfully empty"
	// when the fallback result is empty.
	extractFn func(lang, source, store string) (ExtractResult, bool, error)

	// nodeCountFn, when non-nil, replaces the stored node-count lookup used
	// by the keep-old-index decision (S1). Test seam for the count-query
	// failure path.
	nodeCountFn func(path string) (int, error)

	// readFileFn replaces os.ReadFile in the content-hash gate and in
	// indexFile's nil-data path (F4 test seam): lets tests count how many
	// times a file is read on one pass and assert the hash-gate bytes are the
	// same bytes indexFile indexes (no second read).
	readFileFn func(path string) ([]byte, error)

	// setSchemaRevisionFn replaces the schema-revision marking at the end of
	// RebuildAll (test seam for the failure path — a DB that cannot be
	// marked must never let RebuildAll fake success; production uses
	// db.SetSchemaRevision).
	setSchemaRevisionFn func() error

	// partialFails counts files whose extraction failed with the old index
	// kept (ErrKeepOldIndex) during index passes (M7). Guarded by partialMu;
	// read via PartialFailures(). Reset at the start of each pass so the
	// count reflects only the most recent pass (stale counts would otherwise
	// keep warning forever after one permanent failure).
	partialMu    sync.Mutex
	partialFails int
	// indexFailed lists the paths that failed during the most recent
	// IndexChanges pass (ErrKeepOldIndex partial failures, stat errors,
	// index-delete errors). Guarded by partialMu; read via
	// IndexFailedFiles(). The watcher uses it to requeue only the files that
	// actually failed instead of burning the whole batch's retry budget.
	indexFailed []string
}

// NewOrchestrator creates a new extraction orchestrator.
func NewOrchestrator(database *db.DB, workdir string) *Orchestrator {
	return &Orchestrator{db: database, workdir: workdir}
}

// SetDone wires a shutdown signal channel into the index loops. After the
// channel is closed, in-flight index passes wind down within one unit of work
// (one file, one walk callback).
func (o *Orchestrator) SetDone(done <-chan struct{}) {
	o.done = done
}

// interrupted reports whether the shutdown channel has been closed. nil
// channel → never interrupted.
func (o *Orchestrator) interrupted() bool {
	if o.done == nil {
		return false
	}
	select {
	case <-o.done:
		return true
	default:
		return false
	}
}

// SetForceReindex makes the next IndexAll/IndexChanges ignore mtime short-circuit.
func (o *Orchestrator) SetForceReindex(v bool) {
	o.forceMu.Lock()
	defer o.forceMu.Unlock()
	o.force = v
}

func (o *Orchestrator) isForce() bool {
	o.forceMu.Lock()
	defer o.forceMu.Unlock()
	return o.force
}

// fileNodeCount returns the stored node_count for store, honoring the
// nodeCountFn test seam.
func (o *Orchestrator) fileNodeCount(store string) (int, error) {
	if o.nodeCountFn != nil {
		return o.nodeCountFn(store)
	}
	return o.db.GetFileNodeCount(store)
}

// splitNameLineKey parses keys produced as fmt.Sprintf("%s:%d", name, line).
func splitNameLineKey(key string) (name string, line int, ok bool) {
	// name may contain ':' (rare); split from the right.
	i := len(key) - 1
	for i >= 0 && key[i] >= '0' && key[i] <= '9' {
		i--
	}
	if i < 0 || key[i] != ':' || i+1 >= len(key) {
		return "", 0, false
	}
	n := 0
	for _, c := range key[i+1:] {
		n = n*10 + int(c-'0')
	}
	return key[:i], n, true
}

// bareRank scores node kinds for same-file bare-name lookup.
// Higher wins so listUsers(function) beats a coincidental non-callable.
func bareRank(kind string) int {
	switch kind {
	case "function", "method", "component", "constructor":
		return 3
	case "class", "struct", "interface", "type":
		return 2
	case "route":
		return 1
	default:
		return 0
	}
}

// simplifyHandlerName turns framework handler expressions into a bare symbol.
// Examples: listUsers, pkg.Handler, (*User).Create, UsersController@index, h.Serve
func simplifyHandlerName(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return h
	}
	// Laravel-style Controller@action
	if i := strings.LastIndex(h, "@"); i >= 0 && i+1 < len(h) {
		return h[i+1:]
	}
	// Strip (*T).Method / T.Method / pkg.Func
	if i := strings.LastIndex(h, "."); i >= 0 && i+1 < len(h) {
		return strings.Trim(h[i+1:], "() ")
	}
	// Drop trailing ()
	h = strings.TrimSuffix(h, "()")
	return strings.Trim(h, " ")
}

// linkSameFileOrPark was replaced by the batched indexFile (A4): same-file
// links and cross-file parks are now collected and written atomically in
// ReplaceFileIndex. Keep parkUnresolved's logic inline there (see link()).

// maxIndexFileSize skips oversized blobs (minified bundles, generated dumps).
const maxIndexFileSize = 1 * 1024 * 1024

// ErrIndexInterrupted aborts an in-flight index pass when the shutdown
// channel (SetDone) is closed. Callers must treat it as "shutdown happened;
// partial results are on disk" — never as a retryable failure and never as
// success: the schema revision must not be marked, or the next startup would
// trust a half index (NeedsRebuild would stay false).
// errors.Is(err, ErrIndexInterrupted) identifies it through any wrapping.
var ErrIndexInterrupted = errors.New("index interrupted by shutdown")

// ErrKeepOldIndex marks a file whose extraction failed but whose previous
// index was kept (A4/S1). It is non-fatal: the pass continues with other
// files and the old symbols stay queryable. runIndexJobs counts it as a
// partial failure (M7) instead of a hard error, and because the file meta is
// deliberately NOT refreshed on this path (M2) the next index pass retries
// the file — a broken file self-heals once the parser can read it again.
var ErrKeepOldIndex = errors.New("extraction failed, keeping old index")

// visitIndexable walks the workspace once, applying the shared skip rules, and
// invokes fn for each language-supported source file under the size limit.
// Walk errors on individual paths are skipped so one bad path cannot abort the scan.
func (o *Orchestrator) visitIndexable(fn func(path string, info os.FileInfo, lang string) error) (walkErrs int, err error) {
	err = filepath.Walk(o.workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			walkErrs++
			return nil
		}
		if o.interrupted() {
			return ErrIndexInterrupted
		}
		if info.IsDir() {
			if ShouldSkipDirIn(o.workdir, path, info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// filepath.Walk uses Lstat, so a symlink to a file shows up here with
		// ModeSymlink set. Skip it: the target may live outside the workspace
		// and reading it would leak external content (aligned with
		// internal/tools/node.go safeReadPath's protection).
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.Size() > maxIndexFileSize {
			return nil
		}
		lang := DetectLanguage(path)
		if lang == "" || !IsSupportedLanguage(lang) {
			return nil
		}
		return fn(path, info, lang)
	})
	return walkErrs, err
}

// storePath is the workdir-relative index key for a filesystem path.
func (o *Orchestrator) storePath(path string) string {
	return db.StoragePath(o.workdir, path)
}

// indexIfNeeded reindexes path when the DB says it is stale.
// Returns (1, nodeCount) on success; (0,0,nil) when skipped; a non-nil error
// when the file failed to index (callers aggregate errors, keep going — A8).
// path is a filesystem path (usually absolute from Walk/watcher); the DB key is relative.
//
// A5 incremental gate: when size+mtime are unchanged the cheap metadata check
// cannot see same-size same-mtime edits, so the file content is hashed and
// compared with the stored hash — identical content skips, anything else
// reindexes. Metadata changes reindex directly without an extra read.
//
// F4: the bytes read for the content-hash gate are handed to indexFile so the
// file is never read twice on the same pass.
func (o *Orchestrator) indexIfNeeded(path string, info os.FileInfo, lang string) (files int, nodes int, err error) {
	// Second defense against symlinks (watcher entries bypass the Walk skip):
	// never index a symlink itself — its target may live outside the
	// workspace. Lstat is used because the info passed in may already have
	// followed the link (H2).
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return 0, 0, nil
	}
	key := o.storePath(path)
	var data []byte
	if !o.isForce() {
		needsReindex, err := o.db.FileNeedsReindex(key, info.Size(), float64(info.ModTime().UnixMilli()))
		if err != nil {
			return 0, 0, err
		}
		if !needsReindex {
			// F4: assign to the outer data (declared above), NOT := — a new
			// inner variable would shadow it and indexFile would re-read the
			// file instead of reusing these bytes.
			var rerr error
			data, rerr = o.readFile(path)
			if rerr != nil {
				return 0, 0, rerr
			}
			same, herr := o.db.FileHasContentHash(key, hashContent(data))
			if herr != nil {
				return 0, 0, herr
			}
			if same {
				return 0, 0, nil
			}
		}
	}
	nodeCount, err := o.indexFile(path, lang, data)
	if err != nil {
		return 0, 0, err
	}
	return 1, nodeCount, nil
}

// indexWorkerCount picks how many files to extract/index in parallel.
// DB writes are serialized by db.DB's mutex; only CPU-bound extract runs free.
func indexWorkerCount() int {
	return config.IndexWorkers()
}

type indexJob struct {
	path string
	info os.FileInfo
	lang string
}

// runIndexJobs fans out indexIfNeeded across a small worker pool.
// Errors are aggregated (A8): indexing continues file-by-file, partial results
// stay written, and the joined error is returned at the end.
func (o *Orchestrator) runIndexJobs(jobs []indexJob, onEach func(done, total int)) (int, int, error) {
	if len(jobs) == 0 {
		return 0, 0, nil
	}
	workers := indexWorkerCount()
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers <= 1 {
		totalFiles, totalNodes := 0, 0
		var errs []error
		for i, j := range jobs {
			if o.interrupted() {
				return totalFiles, totalNodes, errors.Join(errs...)
			}
			f, n, err := o.indexIfNeeded(j.path, j.info, j.lang)
			totalFiles += f
			totalNodes += n
			if err != nil {
				// ErrKeepOldIndex is a partial failure (old index kept, will be
				// retried) — count it, don't fail the pass (M7).
				if errors.Is(err, ErrKeepOldIndex) {
					o.partialMu.Lock()
					o.partialFails++
					o.partialMu.Unlock()
				} else {
					errs = append(errs, fmt.Errorf("index %s: %w", j.path, err))
				}
			}
			if onEach != nil {
				onEach(i+1, len(jobs))
			}
		}
		return totalFiles, totalNodes, errors.Join(errs...)
	}

	var (
		mu         sync.Mutex
		totalFiles int
		totalNodes int
		done       int
		wg         sync.WaitGroup
		errs       []error
		ch         = make(chan indexJob, workers*2)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				func(j indexJob) {
					defer func() {
						if r := recover(); r != nil {
							// Keep the immediate per-file diagnostic (path + panic
							// value) so worker-side failures are visible in logs
							// even when the aggregated error is only surfaced
							// much later (H5).
							log.Printf("index worker panic: %s: %v", j.path, r)
							// Count the panic as an index error for this file (H5):
							// IndexAll must return a non-nil error so callers never
							// mark the schema revision on a pass that lost a file.
							mu.Lock()
							errs = append(errs, fmt.Errorf("index %s: panic: %v", j.path, r))
							done++
							cur, tot := done, len(jobs)
							mu.Unlock()
							if onEach != nil {
								onEach(cur, tot)
							}
						}
					}()
					if o.interrupted() {
						return // shutdown: drain the queue without indexing
					}
					f, n, err := o.indexIfNeeded(j.path, j.info, j.lang)
					mu.Lock()
					totalFiles += f
					totalNodes += n
					if err != nil {
						// ErrKeepOldIndex is a partial failure (old index kept, will
						// be retried) — count it, don't fail the pass (M7).
						if errors.Is(err, ErrKeepOldIndex) {
							o.partialMu.Lock()
							o.partialFails++
							o.partialMu.Unlock()
						} else {
							errs = append(errs, fmt.Errorf("index %s: %w", j.path, err))
						}
					}
					done++
					cur, tot := done, len(jobs)
					mu.Unlock()
					if onEach != nil {
						onEach(cur, tot)
					}
				}(j)
			}
		}()
	}
	for _, j := range jobs {
		if o.interrupted() {
			break // stop feeding; workers drain the rest without indexing
		}
		ch <- j
	}
	close(ch)
	wg.Wait()
	return totalFiles, totalNodes, errors.Join(errs...)
}

// collectIndexJobs walks the workspace once into a job list.
// An interrupted walk returns the partial job list wrapped in
// ErrIndexInterrupted; the caller surfaces the sentinel instead of treating
// the partial list as a complete pass.
func (o *Orchestrator) collectIndexJobs() (jobs []indexJob, walkErrs int, err error) {
	walkErrs, err = o.visitIndexable(func(path string, info os.FileInfo, lang string) error {
		jobs = append(jobs, indexJob{path: path, info: info, lang: lang})
		return nil
	})
	return jobs, walkErrs, err
}

// IndexAll indexes all files in the workspace (skips unchanged unless force).
// A8: per-file failures are aggregated — indexing continues, partial results
// are kept, and a non-nil error is returned when any file failed.
// Shutdown (done closed) winds the pass down within one unit of work and
// skips the resolution/synthesis tail. Every interrupted exit returns
// ErrIndexInterrupted (possibly joined with per-file errors) so callers can
// tell "shutdown aborted the pass" from a clean partial success — and must
// NOT mark the schema revision on it.
func (o *Orchestrator) IndexAll() (int, int, error) {
	jobs, walkErrs, err := o.collectIndexJobs()
	if err != nil {
		return 0, 0, err
	}
	if o.interrupted() {
		return 0, 0, ErrIndexInterrupted
	}
	var pruneErr error
	if walkErrs == 0 {
		pruneErr = o.pruneMissingFiles(jobs)
	}
	// M7: the partial-failure count is per-pass; reset so a stale count from
	// an earlier pass cannot trigger a warning on a clean pass.
	o.partialMu.Lock()
	o.partialFails = 0
	o.indexFailed = nil
	o.partialMu.Unlock()
	totalFiles, totalNodes, jerr := o.runIndexJobs(jobs, func(done, total int) {
		if done%500 == 0 {
			log.Printf("indexed progress %d/%d candidates", done, total)
		}
	})
	jerr = errors.Join(pruneErr, jerr)
	if o.interrupted() {
		return totalFiles, totalNodes, errors.Join(ErrIndexInterrupted, jerr)
	}

	// Step 3: turn pending unresolved_refs into cross-file edges.
	if st, rerr := resolution.ResolveAll(o.db, o.workdir); rerr != nil {
		log.Printf("resolve all: %v", rerr)
		jerr = errors.Join(jerr, rerr)
	} else if st.Resolved > 0 || st.Failed > 0 {
		log.Printf("resolved %d edges (%d failed, %d retried)", st.Resolved, st.Failed, st.Retried)
	}
	if o.interrupted() {
		return totalFiles, totalNodes, errors.Join(ErrIndexInterrupted, jerr)
	}
	// Step 7: dynamic-dispatch synthesis (callback / React / bridge…).
	if serr := o.runSynthesis(nil); serr != nil {
		jerr = errors.Join(jerr, serr)
	}
	if n := o.PartialFailures(); n > 0 {
		// Neutral per-pass report, same position as IndexAllWithProgress:
		// whether the schema revision gets marked is the server's (caller's)
		// decision, so only state what happened here — n file(s) failed
		// extraction and will be retried next pass. Do not claim "marked
		// current" (a shutdown between this log and the caller's
		// SetSchemaRevision would mislead operators) nor "revision not
		// marked" (the server's "index warning" log covers the hard-error
		// case, so repeating it here would be redundant).
		log.Printf("index pass: %d file(s) failed extraction; will retry on next pass", n)
	}
	return totalFiles, totalNodes, jerr
}

// runSynthesis runs noise scrubbing + dynamic-dispatch synthesis.
// When files is non-nil only refs related to those files are scrubbed;
// nil means scrub all. SynthesizeAll is always full-table (its bottom pass).
func (o *Orchestrator) runSynthesis(files []string) error {
	// Drop pure-noise failed/pending refs with no project symbol first.
	o.scrubNoise(files)
	st, err := resolution.SynthesizeAll(o.db, o.workdir)
	if err != nil {
		log.Printf("synthesize: %v", err)
		return err
	}
	if st.Written > 0 {
		log.Printf("synthesized %d edges %v", st.Written, st.ByPass)
	}
	return nil
}

// scrubNoise drops failed/pending pure-noise refs with no matching symbol.
// When files is non-nil, only refs belonging to those files are examined;
// nil means scrub all. hasProjectSymbol results are cached per-call.
func (o *Orchestrator) scrubNoise(files []string) {
	n, err := ScrubNoisyFailedRefsForFiles(o.db, files)
	if err != nil {
		log.Printf("scrub noisy refs: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scrubbed %d noisy unresolved refs", n)
	}
}

// RebuildAll wipes the symbol index and force-reindexes everything.
func (o *Orchestrator) RebuildAll() (int, int, error) {
	if err := o.db.WipeIndex(); err != nil {
		return 0, 0, err
	}
	o.forceMu.Lock()
	o.force = true
	o.forceMu.Unlock()
	defer func() {
		o.forceMu.Lock()
		o.force = false
		o.forceMu.Unlock()
	}()
	files, nodes, err := o.IndexAll()
	if err != nil {
		// Interrupted (ErrIndexInterrupted) or a real indexing failure: the
		// wipe already happened but the reindex is incomplete — never mark
		// the schema revision here. The revision stays at the old value, so
		// the next startup's NeedsRebuild()==true triggers a full rebuild
		// instead of trusting an empty/half index.
		return files, nodes, err
	}
	if err := o.markSchemaRevision(); err != nil {
		// The reindex itself succeeded, but the index cannot be certified as
		// current: returning nil here would fake success — the caller would
		// trust an index whose revision was never marked (and a later
		// NeedsRebuild may or may not catch it). Propagate the failure so the
		// caller can surface it.
		return files, nodes, err
	}
	return files, nodes, nil
}

// markSchemaRevision records that the index matches the current extractor
// semantics, honoring the setSchemaRevisionFn test seam.
func (o *Orchestrator) markSchemaRevision() error {
	if o.setSchemaRevisionFn != nil {
		return o.setSchemaRevisionFn()
	}
	return o.db.SetSchemaRevision()
}

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

func hashContent(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readFile returns file contents, honoring the readFileFn test seam.
func (o *Orchestrator) readFile(path string) ([]byte, error) {
	if o.readFileFn != nil {
		return o.readFileFn(path)
	}
	return os.ReadFile(path)
}

// PartialFailures returns how many files failed extraction (old index kept,
// ErrKeepOldIndex) during the most recent index pass. Reset at the start of
// every IndexAll/IndexAllWithProgress/IndexChanges pass, so the value is only
// meaningful right after a pass returned (M7).
func (o *Orchestrator) PartialFailures() int {
	o.partialMu.Lock()
	defer o.partialMu.Unlock()
	return o.partialFails
}

// recordIndexFailure appends one failed path to the current pass's
// IndexFailedFiles list. Only called from IndexChanges (the watcher's
// reindex path); IndexAll/IndexAllWithProgress run parallel jobs whose
// failures are reported via PartialFailures only.
func (o *Orchestrator) recordIndexFailure(path string) {
	o.partialMu.Lock()
	o.indexFailed = append(o.indexFailed, path)
	o.partialMu.Unlock()
}

// IndexFailedFiles returns the paths that failed during the most recent
// IndexChanges pass: extraction partial failures (ErrKeepOldIndex), stat
// errors, and index-delete errors. Reset at the start of every pass like
// PartialFailures, so the value is only meaningful right after a pass
// returned. The watcher requeues only these paths instead of burning the
// whole batch's retry budget on one permanent failure.
func (o *Orchestrator) IndexFailedFiles() []string {
	o.partialMu.Lock()
	defer o.partialMu.Unlock()
	out := make([]string, len(o.indexFailed))
	copy(out, o.indexFailed)
	return out
}

// indexFile extracts and writes the index for one file. data, when non-nil,
// is the file content already read by the caller (indexIfNeeded's content-hash
// gate, F4) and is reused instead of reading the file a second time; nil means
// read from disk. contentHash is always derived from the same bytes that get
// indexed, so the hash gate and the stored hash can never disagree.
func (o *Orchestrator) indexFile(path string, lang string, data []byte) (int, error) {
	if data == nil {
		var err error
		data, err = o.readFile(path)
		if err != nil {
			return 0, err
		}
	}
	contentHash := hashContent(data)
	// Portable index key: workdir-relative.
	store := o.storePath(path)

	// Content-hash short-circuit: mtime can change without edits (touch, checkout).
	// Skip clear+re-extract when the bytes are identical (unless force rebuild).
	if !o.isForce() {
		if same, herr := o.db.FileHasContentHash(store, contentHash); herr == nil && same {
			if info, serr := os.Stat(path); serr == nil {
				// Refresh size/mtime so the cheap FileNeedsReindex stays quiet.
				_ = o.db.TouchFileMeta(store, info.Size(), float64(info.ModTime().UnixMilli()), contentHash)
			}
			return 0, nil
		}
	}

	// Extract: tree-sitter first, regex fallback. On total failure the previous
	// index is KEPT (no ClearFile, no empty file record) and IndexAll
	// continues with other files (A4/Critical#3). The file meta is deliberately
	// NOT touched on this path (M2): a fresh content_hash would make the
	// gates treat the file as current, so an unchanged broken file would be
	// skipped forever. Leaving the stale meta makes the next IndexAll retry
	// the file (self-healing). ErrKeepOldIndex is non-fatal (M7).
	// Normalize once for every consumer (extractors, route detection, bridge
	// detection): strip BOM and normalize CRLF so anchors/line math agree.
	source := normalizeSource(string(data))
	result, tsErrored, extractErr := o.extractFile(lang, source, store)
	// T5: tree-sitter TIMEOUT is not a parse verdict — the regex fallback can
	// yield partial/noisy results that would silently degrade a previously
	// complete tree-sitter index (and the fresh content hash would stop any
	// retry, so the quality loss would be permanent). Keep the old index when
	// one exists; only a file with no prior index gets the weak fallback
	// written (something beats nothing on the first pass).
	if errors.Is(extractErr, ErrTSParseTimeout) {
		if old, cerr := o.fileNodeCount(store); cerr != nil || old > 0 {
			log.Printf("warning: tree-sitter parse timed out for %s, keeping existing index", path)
			// No meta touch (M2): the stale meta makes the next IndexAll
			// retry the file; ErrKeepOldIndex keeps the pass going (M7).
			return 0, ErrKeepOldIndex
		}
		extractErr = nil
	}
	if extractErr != nil {
		log.Printf("warning: extraction failed for %s: %v (keeping existing index)", path, extractErr)
		return 0, ErrKeepOldIndex
	}
	// S1: tree-sitter reported a real parse error AND the regex fallback
	// found nothing (nodes+edges+refs all zero) while the file still has an
	// old index with symbols — the file is unparseable, not empty. Clearing
	// it would destroy the previous symbols, so treat it as an extraction
	// failure: keep the old index, log a warning, do NOT touch the file meta
	// (M2: the stale meta makes the next IndexAll retry the file) and return
	// ErrKeepOldIndex so IndexAll continues but the failure stays visible
	// (M7). The same applies when the stored node-count query itself fails
	// (cerr != nil): the old index could not be inspected, so never gamble
	// on clearing it. A successful tree-sitter parse with an empty result
	// (file genuinely cleared) still clears the old index as before.
	if tsErrored && len(result.Nodes) == 0 && len(result.Edges) == 0 && len(result.Refs) == 0 {
		if old, cerr := o.fileNodeCount(store); cerr != nil || old > 0 {
			log.Printf("warning: extraction failed for %s (tree-sitter error, regex found nothing), keeping existing index", path)
			// No meta touch here either (M2): the stale meta makes the next
			// IndexAll retry this file; ErrKeepOldIndex keeps the pass going.
			return 0, ErrKeepOldIndex
		}
	}
	nodes := result.Nodes
	edges := result.Edges
	refs := result.Refs

	// Detect framework routes (linked to handlers after node insert).
	detector := NewFrameworkDetector()
	routes := detector.DetectRoutes(source, store, lang)
	for _, route := range routes {
		handler := simplifyHandlerName(strings.TrimSpace(route.Handler))
		nodes = append(nodes, ExtractedNode{
			Kind:          "route",
			Name:          route.Method + " " + route.Path,
			File:          route.File,
			Line:          route.Line,
			EndLine:       route.Line,
			Body:          handler,
			Language:      lang,
			QualifiedName: route.QualifiedName,
		})
	}

	// ---- Build the atomic batch. File node first (index 0), then symbols. ----
	dbNodes := make([]db.Node, 0, len(nodes)+1)
	dbNodes = append(dbNodes, db.Node{
		Kind: db.KindFile, Name: store, File: store, Line: 0, Language: lang,
	})
	for _, n := range nodes {
		dbNodes = append(dbNodes, db.Node{
			Kind:          n.Kind,
			Name:          n.Name,
			File:          store,
			Line:          n.Line,
			EndLine:       n.EndLine,
			Body:          n.Body,
			Language:      lang,
			QualifiedName: n.QualifiedName,
			Signature:     n.Signature,
			Docstring:     n.Docstring,
			Visibility:    n.Visibility,
			IsExported:    n.IsExported,
			ReturnType:    n.ReturnType,
			StartColumn:   n.StartColumn,
			EndColumn:     n.EndColumn,
		})
	}
	// Dedup on the nodes conflict key (file,line,kind,name): UpsertNode would
	// merge duplicates into one row; keep the first occurrence so batch ids
	// stay 1:1 with dbNodes.
	{
		seen := make(map[string]struct{}, len(dbNodes))
		kept := dbNodes[:0]
		for _, n := range dbNodes {
			k := fmt.Sprintf("%s\x00%d\x00%s\x00%s", n.File, n.Line, n.Kind, n.Name)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			kept = append(kept, n)
		}
		dbNodes = kept
	}

	// name:line → dbNodes index; bare-name best-hit map (same ranking as before).
	// Names defined at more than one (line, rank) spot are ambiguous and are
	// deliberately NOT registered as bare-name hits: same-file linking must
	// never first-wins across two same-named symbols (audit: two String()
	// methods on different receivers stole each other's calls).
	nodeIdx := make(map[string]int, len(dbNodes))
	type bareHit struct{ idx, rank, line int }
	bareBest := map[string]bareHit{}
	ambiguous := map[string]bool{}
	nameIdx := map[string][]int{}
	for i := 1; i < len(dbNodes); i++ { // skip the file node
		n := dbNodes[i]
		nodeIdx[fmt.Sprintf("%s:%d", n.Name, n.Line)] = i
		if n.Kind == "signature" {
			// Interface method signatures can be neither a call source nor a
			// call target; keep them out of the link maps entirely.
			continue
		}
		nameIdx[n.Name] = append(nameIdx[n.Name], i)
		prev, ok := bareBest[n.Name]
		rank := bareRank(n.Kind)
		if ok && (prev.line != n.Line || prev.rank != rank) {
			ambiguous[n.Name] = true
		}
		if !ok || rank > prev.rank || (rank == prev.rank && n.Line < prev.line) {
			bareBest[n.Name] = bareHit{idx: i, rank: rank, line: n.Line}
		}
	}
	sameFileBare := make(map[string]int, len(bareBest))
	for name, hit := range bareBest {
		if ambiguous[name] {
			continue
		}
		// Bare name → best unambiguous hit (compatibility with older indexes).
		nodeIdx[name] = hit.idx
		sameFileBare[name] = hit.idx
	}

	// ph(i) is the batch placeholder id for dbNodes[i] (negative; resolved to
	// the real row id inside ReplaceFileIndex's transaction).
	ph := func(i int) int64 { return -int64(i + 1) }

	dbEdges := make([]db.Edge, 0, len(edges)+len(routes))
	var parks []db.UnresolvedRef
	link := func(from int, targetName string, line, col int, kind string) {
		if from < 0 || targetName == "" {
			return
		}
		if tid, ok := sameFileBare[targetName]; ok && tid > 0 {
			dbEdges = append(dbEdges, db.Edge{
				SourceID:   ph(from),
				TargetID:   ph(tid),
				Kind:       kind,
				File:       store,
				Line:       line,
				Col:        col,
				Provenance: "exact",
			})
			return
		}
		// Ambiguous same-file name (multiple definitions): link only when
		// exactly one definition's source range contains the reference line.
		// Any other case parks the ref so the resolution pass can decide
		// (and refuses when the candidates still tie).
		if idxs := nameIdx[targetName]; len(idxs) > 1 {
			hit := -1
			contain := 0
			for _, idx := range idxs {
				n := dbNodes[idx]
				if n.Kind == db.KindFile || n.Kind == "module" || n.Kind == "signature" {
					continue
				}
				end := n.EndLine
				if end == 0 {
					end = n.Line
				}
				if n.Line <= line && line <= end {
					contain++
					hit = idx
				}
			}
			if contain == 1 {
				dbEdges = append(dbEdges, db.Edge{
					SourceID:   ph(from),
					TargetID:   ph(hit),
					Kind:       kind,
					File:       store,
					Line:       line,
					Col:        col,
					Provenance: "exact",
				})
				return
			}
		}
		if !ShouldParkRef(o.db, targetName) {
			return
		}
		parks = append(parks, db.UnresolvedRef{
			FromNode:      ph(from),
			ReferenceName: targetName,
			ReferenceKind: kind,
			Line:          line,
			Col:           col,
			FilePath:      store,
			Language:      lang,
			Status:        "pending",
			NameTail:      NameTail(targetName),
		})
	}

	// Route → handler: same-file edge now; cross-file → unresolved_refs.
	for _, route := range routes {
		handler := simplifyHandlerName(route.Handler)
		if handler == "" {
			continue
		}
		routeKey := fmt.Sprintf("%s:%d", route.Method+" "+route.Path, route.Line)
		routeIdx, ok := nodeIdx[routeKey]
		if !ok {
			continue
		}
		link(routeIdx, handler, route.Line, 0, db.EdgeReferences)
	}

	// Pre-resolve import targets: module nodes live outside this file, so they
	// are looked up before the batch. New module nodes are NOT created here —
	// they are queued as placeholders and upserted inside ReplaceFileIndex's
	// transaction (F5), so a failed batch rolls back and leaves no orphaned
	// module nodes behind.
	importTarget := make(map[int]int64, len(edges))
	moduleNodes := make([]db.Node, 0, len(edges))
	moduleIdx := make(map[string]int, len(edges))
	for i, e := range edges {
		if e.Kind != "imports" {
			continue
		}
		targetNodes, gerr := o.db.GetNodeByName(e.TargetName)
		if gerr != nil {
			log.Printf("lookup import target %s: %v", e.TargetName, gerr)
		}
		var targetID int64
		if len(targetNodes) > 0 {
			// Prefer a module node so import edges don't attach to a
			// same-named function/class by accident.
			targetID = targetNodes[0].ID
			for _, tn := range targetNodes {
				if tn.Kind == "module" {
					targetID = tn.ID
					break
				}
			}
		} else {
			// Queue a module node for creation inside the batch transaction.
			// Its placeholder id -(len(dbNodes)+idx+1) is resolved to the real
			// row id by ReplaceFileIndex (module placeholders live just below
			// the batch-node placeholder range).
			idx, ok := moduleIdx[e.TargetName]
			if !ok {
				idx = len(moduleNodes)
				moduleIdx[e.TargetName] = idx
				moduleNodes = append(moduleNodes, db.Node{
					Kind:     "module",
					Name:     e.TargetName,
					File:     e.TargetName,
					Line:     0,
					Language: lang,
				})
			}
			targetID = -int64(len(dbNodes) + idx + 1)
		}
		if targetID != 0 {
			importTarget[i] = targetID
		}
	}

	// Structural edges (imports, etc.)
	for i, e := range edges {
		if e.Kind == "imports" {
			if tid := importTarget[i]; tid != 0 {
				dbEdges = append(dbEdges, db.Edge{
					SourceID:   ph(0),
					TargetID:   tid,
					Kind:       "imports",
					File:       store,
					Line:       e.Line,
					Provenance: "exact",
				})
			}
			continue
		}
		srcIdx := o.resolveSourceIdx(e.SourceName, e.Line, nodeIdx)
		if srcIdx < 0 {
			continue
		}
		if e.Kind == "calls" {
			link(srcIdx, e.TargetName, e.Line, e.Col, db.EdgeCalls)
			continue
		}
		// extends/implements: same-file only for now; else park.
		link(srcIdx, e.TargetName, e.Line, e.Col, e.Kind)
	}

	// Pending call/type refs from extractors.
	for _, ref := range refs {
		srcIdx := o.resolveSourceIdx(ref.FromName, ref.FromLine, nodeIdx)
		if srcIdx < 0 {
			srcIdx = 0 // file node
		}
		kind := ref.ReferenceKind
		if kind == "" {
			kind = db.EdgeCalls
		}
		link(srcIdx, ref.ReferenceName, ref.Line, ref.Col, kind)
	}

	// Cross-language bridges: same-file source; target may be foreign placeholder
	// (still written as edge so bridge tooling keeps working; full resolution in step 7).
	bridgeDetector := NewCrossLanguageDetector()
	bridges := bridgeDetector.Detect(source, store, lang)
	for _, bridge := range bridges {
		targetName := strings.TrimSpace(bridge.TargetFunc)
		if targetName == "" {
			continue
		}
		srcIdx := -1
		bestLine := -1
		for _, n := range nodes {
			if n.Kind != "function" && n.Kind != "method" {
				continue
			}
			end := n.EndLine
			if end == 0 {
				end = n.Line
			}
			if n.Line <= bridge.Line && bridge.Line <= end && n.Line >= bestLine {
				if idx, ok := nodeIdx[fmt.Sprintf("%s:%d", n.Name, n.Line)]; ok {
					srcIdx = idx
					bestLine = n.Line
				}
			}
		}
		if srcIdx < 0 {
			srcIdx = 0 // file node
		}
		link(srcIdx, targetName, bridge.Line, 0, "bridge")
	}

	// Single-transaction replace: clear old rows + insert batch + file record.
	fr := db.FileRecord{
		Path:        store,
		ContentHash: contentHash,
		Language:    lang,
		NodeCount:   len(nodes),
	}
	if info, serr := os.Stat(path); serr == nil {
		fr.Size = info.Size()
		fr.Mtime = float64(info.ModTime().UnixMilli())
	} else {
		log.Printf("stat after index %s: %v", path, serr)
	}
	// Re-index deletes this file's nodes and CASCADE-drops inbound edges
	// from other files. Park those edges as pending refs first so
	// ResolveForFiles can rebuild them against the new node ids.
	if perr := o.db.ParkInboundRefsForFile(store); perr != nil {
		return 0, perr
	}
	if _, werr := o.db.ReplaceFileIndex(store, dbNodes, dbEdges, parks, &fr, moduleNodes...); werr != nil {
		return 0, werr
	}

	return len(nodes), nil
}

// extractFile runs the extraction pipeline: tree-sitter preferred, regex
// fallback when tree-sitter fails. Returns the result, whether tree-sitter
// itself errored (false when it succeeded, was unavailable, or regex is the
// only extractor), and an error only when every extractor failed (A4) —
// callers then keep the previous index for the file. One exception: when
// tree-sitter TIMED OUT (ErrTSParseTimeout) and the regex fallback produced
// a non-empty result, the sentinel is returned as the error so callers can
// tell "timeout" from a genuine parse verdict and keep a good old index
// instead of overwriting it with weak regex partial results (must-fix).
func (o *Orchestrator) extractFile(lang, source, store string) (ExtractResult, bool, error) {
	if o.extractFn != nil {
		return o.extractFn(lang, source, store)
	}
	if ts := NewTreeSitterExtractor(lang); ts != nil {
		res, err := ts.Extract(source, store)
		if err == nil {
			return res, false, nil
		}
		log.Printf("tree-sitter extraction failed for %s (%s), falling back to regex: %v", store, lang, err)
		// S1: remember the tree-sitter failure so the caller can tell an
		// unparseable file (fallback also empty) apart from a successfully
		// extracted empty file, and keep the old index in the former case.
		fb, ferr := NewExtractor(lang).Extract(source, store)
		if errors.Is(err, ErrTSParseTimeout) && ferr == nil {
			// Timeout is NOT a parse verdict: signal it via the sentinel so
			// the caller keeps the old index when one exists (see indexFile).
			return fb, true, fmt.Errorf("%w: %s parse timed out, regex fallback used", ErrTSParseTimeout, lang)
		}
		return fb, true, ferr
	}
	res, err := NewExtractor(lang).Extract(source, store)
	return res, false, err
}

// resolveSourceIdx finds the dbNodes index for a ref/edge source name.
// Returns 0 for the file node when name is empty, -1 when no node matches
// (callers decide whether to skip or fall back to the file node).
func (o *Orchestrator) resolveSourceIdx(name string, line int, nodeIdx map[string]int) int {
	if name == "" {
		return 0 // file node
	}
	if line > 0 {
		if idx, ok := nodeIdx[fmt.Sprintf("%s:%d", name, line)]; ok {
			return idx
		}
	}
	if idx, ok := nodeIdx[name]; ok {
		return idx
	}
	// Bare name not found — no better match exists.
	return -1
}

// IndexChanges indexes only files that have changed since last index.
// files are filesystem paths (absolute from watcher/git); resolution uses storage keys.
// A6: paths that no longer exist (deleted / renamed away) drop their old index
// rows instead of being silently skipped. A8: failures are aggregated and
// returned; successfully indexed files keep their partial results.
func (o *Orchestrator) IndexChanges(files []string) (int, int, error) {
	totalFiles := 0
	totalNodes := 0
	storeKeys := make([]string, 0, len(files))
	var errs []error

	// M7: per-pass semantics, same as IndexAll — reset the partial-failure
	// count (and the failed-files list) before the pass so PartialFailures()
	// and IndexFailedFiles() reflect only this batch (a stale count from an
	// earlier pass would keep warning forever).
	o.partialMu.Lock()
	o.partialFails = 0
	o.indexFailed = nil
	o.partialMu.Unlock()

	for _, path := range files {
		if o.interrupted() {
			break // shutdown: stop mid-batch, keep what was written
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				// File was deleted (or renamed away): remove the stale index.
				if derr := o.db.DeleteFile(o.storePath(path)); derr != nil {
					errs = append(errs, fmt.Errorf("delete index %s: %w", path, derr))
					o.recordIndexFailure(path)
				}
			} else {
				errs = append(errs, fmt.Errorf("stat %s: %w", path, err))
				o.recordIndexFailure(path)
			}
			continue
		}
		if info.IsDir() || info.Size() > maxIndexFileSize {
			continue
		}
		lang := DetectLanguage(path)
		if lang == "" || !IsSupportedLanguage(lang) {
			continue
		}
		filesN, nodes, ierr := o.indexIfNeeded(path, info, lang)
		totalFiles += filesN
		totalNodes += nodes
		if ierr != nil {
			// ErrKeepOldIndex is a partial failure (old index kept, will be
			// retried) — count it, don't fail the pass (M7). Aligned with
			// runIndexJobs: without this, the watcher and git-assist paths
			// would log "sync error" for every extraction failure that
			// intentionally kept the old index.
			if errors.Is(ierr, ErrKeepOldIndex) {
				o.partialMu.Lock()
				o.partialFails++
				o.partialMu.Unlock()
			} else {
				errs = append(errs, ierr)
			}
			o.recordIndexFailure(path)
		}
		storeKeys = append(storeKeys, o.storePath(path))
	}

	if o.interrupted() {
		return totalFiles, totalNodes, errors.Join(append(errs, ErrIndexInterrupted)...)
	}
	if _, err := resolution.ResolveForFiles(o.db, o.workdir, storeKeys); err != nil {
		log.Printf("resolve changes: %v", err)
		errs = append(errs, err)
	}
	if serr := o.runSynthesis(storeKeys); serr != nil {
		errs = append(errs, serr)
	}
	return totalFiles, totalNodes, errors.Join(errs...)
}

// ProgressFunc is called during indexing to report progress.
type ProgressFunc func(phase string, current, total int)

// IndexAllWithProgress indexes all files with progress reporting.
// Same walk/skip/index path as IndexAll (collect jobs, then parallel pool).
//
// Thread safety: onProgress may be called concurrently by multiple worker
// goroutines. Callers must ensure their implementation is thread-safe.
func (o *Orchestrator) IndexAllWithProgress(onProgress ProgressFunc) (int, int, error) {
	start := time.Now()
	jobs, walkErrs, err := o.collectIndexJobs()
	if err != nil {
		return 0, 0, err
	}
	if o.interrupted() {
		return 0, 0, ErrIndexInterrupted
	}
	var pruneErr error
	if walkErrs == 0 {
		pruneErr = o.pruneMissingFiles(jobs)
	}
	totalCandidates := len(jobs)
	// M7: per-pass partial-failure count (see IndexAll).
	o.partialMu.Lock()
	o.partialFails = 0
	o.indexFailed = nil
	o.partialMu.Unlock()
	indexedFiles, indexedNodes, jerr := o.runIndexJobs(jobs, func(done, total int) {
		if onProgress != nil && (done%10 == 0 || done == total) {
			onProgress("indexing", done, totalCandidates)
		}
	})
	jerr = errors.Join(pruneErr, jerr)

	if o.interrupted() {
		return indexedFiles, indexedNodes, errors.Join(ErrIndexInterrupted, jerr)
	}
	if st, rerr := resolution.ResolveAll(o.db, o.workdir); rerr != nil {
		log.Printf("resolve all: %v", rerr)
		jerr = errors.Join(jerr, rerr)
	} else if st.Resolved > 0 {
		log.Printf("resolved %d edges after index", st.Resolved)
	}
	if o.interrupted() {
		return indexedFiles, indexedNodes, errors.Join(ErrIndexInterrupted, jerr)
	}
	if serr := o.runSynthesis(nil); serr != nil {
		jerr = errors.Join(jerr, serr)
	}
	if n := o.PartialFailures(); n > 0 {
		// Neutral per-pass report, same wording as IndexAll: the schema-revision
		// decision belongs to the server (caller); say only what happened here
		// — n file(s) failed extraction and will be retried next pass. No
		// "marked current" / "revision not marked" claims (see IndexAll).
		log.Printf("index pass: %d file(s) failed extraction; will retry on next pass", n)
	}
	elapsed := time.Since(start)
	pending, _ := o.db.CountUnresolvedRefs("pending")
	log.Printf("indexing complete: %d files, %d nodes, %d pending refs in %v (workers=%d)",
		indexedFiles, indexedNodes, pending, elapsed, indexWorkerCount())

	return indexedFiles, indexedNodes, jerr
}

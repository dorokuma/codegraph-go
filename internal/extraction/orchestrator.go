package extraction

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// NewOrchestrator creates a new extraction orchestrator.
func NewOrchestrator(database *db.DB, workdir string) *Orchestrator {
	return &Orchestrator{db: database, workdir: workdir}
}

// SetDone wires a shutdown signal channel into the index loops. After the
// channel is closed, in-flight index passes wind down within one unit of work
// (one file, one walk callback).

// SetDone wires a shutdown signal channel into the index loops. After the
// channel is closed, in-flight index passes wind down within one unit of work
// (one file, one walk callback).
func (o *Orchestrator) SetDone(done <-chan struct{}) {
	o.done = done
}

// interrupted reports whether the shutdown channel has been closed. nil
// channel → never interrupted.

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

// maxIndexFileSize skips oversized blobs (minified bundles, generated dumps).
const maxIndexFileSize = 1 * 1024 * 1024

// ErrIndexInterrupted aborts an in-flight index pass when the shutdown
// channel (SetDone) is closed. Callers must treat it as "shutdown happened;
// partial results are on disk" — never as a retryable failure and never as
// success: the schema revision must not be marked, or the next startup would
// trust a half index (NeedsRebuild would stay false).
// errors.Is(err, ErrIndexInterrupted) identifies it through any wrapping.

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

// markSchemaRevision records that the index matches the current extractor
// semantics, honoring the setSchemaRevisionFn test seam.
func (o *Orchestrator) markSchemaRevision() error {
	if o.setSchemaRevisionFn != nil {
		return o.setSchemaRevisionFn()
	}
	return o.db.SetSchemaRevision()
}

// IndexFile indexes a single file.

func hashContent(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readFile returns file contents, honoring the readFileFn test seam.

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

// ProgressFunc is called during indexing to report progress.
type ProgressFunc func(phase string, current, total int)

// IndexAllWithProgress indexes all files with progress reporting.
// Same walk/skip/index path as IndexAll (collect jobs, then parallel pool).
//
// Thread safety: onProgress may be called concurrently by multiple worker
// goroutines. Callers must ensure their implementation is thread-safe.

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

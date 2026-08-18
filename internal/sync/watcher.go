package sync

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dorokuma/codegraph-go/internal/extraction"

	"github.com/fsnotify/fsnotify"
)

// maxPendingRetries bounds how many consecutive failed sync attempts a path
// may survive before the watcher gives up on it. Without a cap, a permanently
// failing path (unreadable file, persistent extraction failure) would be
// requeued forever — a hot retry loop that never converges. A later fs event
// for the same path resets the count and starts a fresh attempt cycle.
const maxPendingRetries = 3

// stopGracePeriod bounds how long Stop waits for an in-flight reindex pass.
// A pass can legitimately take minutes on a large batch; shutdown must not
// hang on it (the orchestrator's own interrupt winds the pass down between
// files). A var so tests can exercise the timeout branch quickly.
var stopGracePeriod = 30 * time.Second

// pendingEntry tracks one queued path: when it became pending and how many
// consecutive sync attempts failed (see maxPendingRetries).
type pendingEntry struct {
	since   time.Time
	retries int
}

// indexer is the subset of *extraction.Orchestrator the watcher depends on.
// An interface (instead of the concrete type) keeps processPending testable
// with fakes for the failure/requeue paths.
// IndexFailedFiles reports which paths failed in the most recent
// IndexChanges pass, so failures can be requeued per-path instead of
// whole-batch (a single permanent failure must not burn the whole batch's
// retry budget).
type indexer interface {
	IndexChanges(files []string) (int, int, error)
	DeleteFile(path string) error
	DeleteTree(path string) error
	PartialFailures() int
	IndexFailedFiles() []string
}

// Watcher watches for file changes and triggers reindexing.
type Watcher struct {
	orchestrator indexer
	workdir      string
	watcher      *fsnotify.Watcher
	mu           sync.Mutex
	pending      map[string]*pendingEntry
	debounce     time.Duration
	done         chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
	// procMu/procRunning/procWG serialize reindex passes and let Stop wait
	// (bounded) for an in-flight pass without blocking on it.
	procMu      sync.Mutex
	procRunning bool
	procWG      sync.WaitGroup
	// dropped counts paths permanently dropped after exhausting the retry
	// budget (see requeue); exposed via DroppedCount so drops are observable
	// outside the logs.
	dropped atomic.Uint64
}

// NewWatcher creates a new file watcher.
func NewWatcher(orch *extraction.Orchestrator, workdir string) (*Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &Watcher{
		orchestrator: orch,
		workdir:      workdir,
		watcher:      w,
		pending:      make(map[string]*pendingEntry),
		debounce:     2 * time.Second,
		done:         make(chan struct{}),
	}, nil
}

// Start begins watching for file changes.
func (w *Watcher) Start() error {
	// Add directories to watch. The initial walk must not silently swallow
	// errors: an unreadable subtree would otherwise be a permanent blind
	// spot (no watch, no events, no reindex). Non-root walk errors are
	// logged and counted; failure at the root itself aborts Start so it
	// surfaces to the caller.
	root := filepath.Clean(w.workdir)
	walkErrs := 0
	err := filepath.Walk(w.workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("watcher: walk %s: %v", path, err)
			walkErrs++
			if filepath.Clean(path) == root {
				return err
			}
			return nil
		}
		if info.IsDir() {
			if extraction.ShouldSkipDirIn(w.workdir, path, info.Name()) {
				return filepath.SkipDir
			}
			if aerr := w.watcher.Add(path); aerr != nil {
				log.Printf("watcher add %s: %v", path, aerr)
				return aerr
			}
		}
		return nil
	})
	if err != nil {
		// Release the fsnotify handle: a failed Start must not leak the
		// inotify/kqueue watches (the caller does not call Stop after a
		// failed Start, so nothing else would ever close them).
		_ = w.watcher.Close()
		return err
	}
	if walkErrs > 0 {
		log.Printf("watcher: %d directories unreadable during initial walk; changes there will not be tracked", walkErrs)
	}

	w.wg.Add(1)
	go w.loop()
	return nil
}

// Stop stops the watcher. Safe to call multiple times.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		_ = w.watcher.Close()
	})
	// Wait for the event loop and any in-flight reindex pass, bounded: a
	// huge pending batch can take minutes, and shutdown must not hang on it
	// (the orchestrator interrupt already winds the pass down between files).
	waitCh := make(chan struct{})
	go func() {
		w.wg.Wait()
		w.procWG.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-time.After(stopGracePeriod):
		log.Printf("watcher: stop timed out waiting for in-flight sync; continuing")
	}
}

// WaitIdle waits for the event loop to exit and every reindex pass already
// running to finish, without a timeout. Stop is bounded by stopGracePeriod so
// shutdown never hangs on a huge batch; WaitIdle is its unbounded complement
// for callers that must not release a resource an in-flight pass still uses
// — the server waits for it before closing the DB, so a pass that outlived
// Stop's grace period can never touch a closed connection (use-after-close
// panic). Call after Stop: passes only start from the event loop, and
// WaitIdle waits for the loop first, so no new pass can start once it
// returns. Safe to call multiple times and concurrently with Stop.
func (w *Watcher) WaitIdle() {
	w.wg.Wait()
	w.procWG.Wait()
}

func (w *Watcher) loop() {
	defer w.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Only care about create, write, remove, rename
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			path := event.Name

			// New directories must be watched recursively (official watcher behavior).
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(path); err == nil && info.IsDir() {
					w.watchTree(path)
					continue
				}
			}

			// Source files always queue. Remove/Rename also queues
			// directories and extensionless paths so DeleteTree can
			// drop the old prefix (DetectLanguage is empty for those).
			if !shouldQueueWatchPath(path, event.Op) {
				continue
			}

			w.mu.Lock()
			w.pending[path] = &pendingEntry{since: time.Now()}
			w.mu.Unlock()

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			// F3: an event overflow means the kernel dropped events — some
			// change was almost certainly missed. Trigger a full rescan of the
			// watch root through the normal pending/debounce path so nothing
			// stays stale; other errors are still logged only.
			if errors.Is(err, fsnotify.ErrEventOverflow) || strings.Contains(err.Error(), "overflow") {
				log.Printf("watcher: event overflow — events may have been lost, triggering full rescan")
				w.rescanAll()
				continue
			}
			log.Printf("watcher error: %v", err)

		case <-ticker.C:
			w.maybeProcessPending()
		}
	}
}

// watchTree recursively adds a newly created directory tree to the watch list.
func (w *Watcher) watchTree(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Same blind-spot rule as Start: log walk failures so an
			// unreadable subtree is at least visible in the logs.
			log.Printf("watcher: walk %s: %v", path, err)
			return nil
		}
		if !info.IsDir() {
			// Queue source files that appeared with the new tree
			lang := extraction.DetectLanguage(path)
			if lang != "" && extraction.IsSupportedLanguage(lang) {
				w.mu.Lock()
				w.pending[path] = &pendingEntry{since: time.Now()}
				w.mu.Unlock()
			}
			return nil
		}
		if extraction.ShouldSkipDirIn(w.workdir, path, info.Name()) {
			return filepath.SkipDir
		}
		if err := w.watcher.Add(path); err != nil {
			log.Printf("watcher add %s: %v", path, err)
		}
		return nil
	})
}

// rescanAll queues every supported source file under the watch root for
// reindexing through the normal pending/debounce path (processPending →
// IndexChanges). Used when fsnotify reports ErrEventOverflow: events may have
// been lost, so a full sweep is the only way to guarantee no change is missed.
// S1: the sweep reuses watchTree's semantics — directories encountered by the
// walk are (re)registered with the fsnotify watcher before their source files
// go to pending. A directory created during the overflow window would
// otherwise never be watched (its events were dropped along with the create),
// a permanent blind spot; re-adding matches the normal Start/walk path and is
// a no-op for already-watched dirs.
func (w *Watcher) rescanAll() {
	w.watchTree(w.workdir)
}

// maybeProcessPending starts one processPending pass if none is currently
// running. Reindex runs off the loop goroutine so Stop never blocks on it and
// fs events keep being consumed during a long pass; the guard preserves the
// old no-overlap behavior of the synchronous ticker branch.
func (w *Watcher) maybeProcessPending() {
	w.procMu.Lock()
	if w.procRunning {
		w.procMu.Unlock()
		return
	}
	w.procRunning = true
	w.procMu.Unlock()

	w.procWG.Add(1)
	go func() {
		defer w.procWG.Done()
		defer func() {
			w.procMu.Lock()
			w.procRunning = false
			w.procMu.Unlock()
		}()
		w.processPending()
	}()
}

func (w *Watcher) isStopped() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// pendingJob is one path picked up by a processPending pass. The entry stays
// in the pending map during processing (removed only when the pass settles or
// fails it), so the retry budget survives across passes and a fresh fs event
// can replace it (new generation, budget reset).
type pendingJob struct {
	path string
	e    *pendingEntry
}

// requeue marks a failed job for retry: the entry keeps its retry count (the
// counter must survive across passes, so the entry is NOT removed at snapshot
// time) and its debounce window is restarted — without that a permanent
// failure would hot-loop every tick. Consecutive failures are capped at
// maxPendingRetries; one more failure drops the path with a log and a
// DroppedCount increment. A fresh fs event that replaced the entry meanwhile
// starts a new generation with a reset budget and is left alone.
func (w *Watcher) requeue(job pendingJob, why string) {
	if w.isStopped() {
		return // shutting down; no point retrying
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cur := w.pending[job.path]
	if cur == nil {
		cur = &pendingEntry{since: time.Now(), retries: 1}
		w.pending[job.path] = cur
		log.Printf("watcher: requeued %s after sync failure (%s), attempt 1", job.path, why)
		return
	}
	if cur != job.e {
		// A new fs event replaced the entry: fresh generation, budget reset.
		return
	}
	cur.retries++
	if cur.retries >= maxPendingRetries {
		delete(w.pending, job.path)
		w.dropped.Add(1)
		log.Printf("watcher: dropped %s after %d consecutive failed sync attempts (%s)", job.path, maxPendingRetries, why)
		return
	}
	cur.since = time.Now()
	log.Printf("watcher: requeued %s after sync failure (%s), attempt %d", job.path, why, cur.retries)
}

// requeueFailed requeues only the jobs whose paths actually failed in the
// just-finished IndexChanges pass (per IndexFailedFiles), and settles the
// rest. The old whole-batch requeue shared one retry budget across every
// path: in a 50-file batch a single permanently failing file would burn all
// 50 paths' retries together and drop healthy files along with it. Fallback:
// when the pass reports no per-path failures (no list available), requeue the
// whole batch as before rather than silently dropping the failure signal.
func (w *Watcher) requeueFailed(jobs []pendingJob, why string) {
	failed := make(map[string]bool)
	for _, p := range w.orchestrator.IndexFailedFiles() {
		failed[p] = true
	}
	if len(failed) == 0 {
		for _, job := range jobs {
			w.requeue(job, why)
		}
		return
	}
	for _, job := range jobs {
		if failed[job.path] {
			w.requeue(job, why)
		} else {
			w.settled(job)
		}
	}
}

// settled removes a successfully processed job from pending — unless a newer
// fs event already replaced its entry (that fresh event is processed by the
// next pass).
func (w *Watcher) settled(job pendingJob) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.pending[job.path]; ok && cur == job.e {
		delete(w.pending, job.path)
	}
}

func (w *Watcher) processPending() {
	w.mu.Lock()
	now := time.Now()
	var ready []pendingJob
	for path, e := range w.pending {
		if now.Sub(e.since) >= w.debounce {
			ready = append(ready, pendingJob{path: path, e: e})
		}
	}
	w.mu.Unlock()

	if len(ready) == 0 {
		return
	}

	// Shutdown check: don't start new work after Stop.
	if w.isStopped() {
		return
	}

	// Filter out deleted files. Lstat (not Stat): a real file replaced by a
	// symlink must NOT be treated as "still exists, reindex" — extraction
	// refuses to index symlinks (indexIfNeeded Lstat-skips them), so the old
	// nodes/edges would linger as ghosts. Symlinks and other non-regular
	// files take the DeleteFile path to clear any stale index.
	var existing []string
	var undecided []pendingJob // jobs whose outcome depends on the batch below
	for _, job := range ready {
		path := job.path
		info, err := os.Lstat(path)
		switch {
		case err == nil && info.IsDir():
			if derr := w.orchestrator.DeleteTree(path); derr != nil {
				log.Printf("delete tree %s: %v", path, derr)
				w.requeue(job, "delete failed")
			} else {
				w.settled(job)
			}
		case err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()):
			if derr := w.orchestrator.DeleteFile(path); derr != nil {
				log.Printf("delete index %s: %v", path, derr)
				w.requeue(job, "delete failed")
			} else {
				w.settled(job)
			}
		case err == nil:
			existing = append(existing, path)
			undecided = append(undecided, job)
		case os.IsNotExist(err):
			// File or directory gone (delete / rename-away): drop the
			// exact key and any children stored under path/.
			if derr := w.orchestrator.DeleteTree(path); derr != nil {
				log.Printf("delete tree %s: %v", path, derr)
				w.requeue(job, "delete failed")
			} else {
				w.settled(job)
			}
		default:
			// Permission error or other transient issue — retry on a later pass
			log.Printf("stat pending %s: %v", path, err)
			w.requeue(job, "stat failed")
		}
	}

	if len(existing) == 0 {
		return
	}

	// Shutdown check again before the (potentially long) reindex pass.
	if w.isStopped() {
		return
	}

	// Reindex changed files.
	fileCount, nodeCount, err := w.orchestrator.IndexChanges(existing)
	if err != nil {
		if errors.Is(err, extraction.ErrIndexInterrupted) {
			return // shutting down; do not requeue
		}
		log.Printf("sync error: %v", err)
		// Requeue only the files that actually failed (unchanged files are
		// skipped cheaply by the mtime gate on the next pass).
		w.requeueFailed(undecided, "index error")
		return
	}
	// ErrKeepOldIndex failures are counted, not returned as errors: the old
	// index is kept and the change would otherwise be silently dropped (the
	// "will retry on next pass" comment in extraction does not hold for the
	// watcher, which has no next full pass). Requeue the failed files only.
	if n := w.orchestrator.PartialFailures(); n > 0 {
		log.Printf("watcher: %d file(s) had partial extraction failures; requeueing for retry", n)
		w.requeueFailed(undecided, "partial failure")
		return
	}

	for _, job := range undecided {
		w.settled(job)
	}
	if fileCount > 0 {
		log.Printf("auto-sync: %d files, %d nodes", fileCount, nodeCount)
	}
}

// PendingFiles returns a list of files waiting to be synced.
func (w *Watcher) PendingFiles() []string {
	files, _, _ := w.PendingFileStats()
	return files
}

// PendingFileStats returns up to maxPendingFiles pending paths (relative to
// workdir), the total number of queued paths, and whether the returned list
// was truncated. Unlike PendingFiles alone, the total lets callers tell a
// large-but-complete queue from a truncated one.
func (w *Watcher) PendingFileStats() (files []string, total int, truncated bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	const maxPendingFiles = 500
	total = len(w.pending)
	rel := func(path string) string {
		r, err := filepath.Rel(w.workdir, path)
		if err != nil || r == "" {
			return path
		}
		return r
	}
	// Deterministic order: map iteration is random and callers display the list.
	paths := make([]string, 0, len(w.pending))
	for p := range w.pending {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	files = make([]string, 0, min(len(paths), maxPendingFiles))
	for _, p := range paths {
		files = append(files, rel(p))
		if len(files) >= maxPendingFiles {
			truncated = total > len(files)
			return files, total, truncated
		}
	}
	return files, total, false
}

// DroppedCount returns how many paths the watcher permanently dropped after
// exhausting maxPendingRetries consecutive sync attempts. Before this
// counter, drops were only visible in the logs; the status tool surfaces it
// so a file that stops syncing entirely is observable.
func (w *Watcher) DroppedCount() uint64 {
	return w.dropped.Load()
}

// AddDir adds a new directory to the watch list.
func (w *Watcher) AddDir(path string) error {
	return w.watcher.Add(path)
}

// RemoveDir removes a directory from the watch list.
func (w *Watcher) RemoveDir(path string) error {
	return w.watcher.Remove(path)
}

// IsSupported returns true if the file is a supported source file.
func IsSupported(path string) bool {
	lang := extraction.DetectLanguage(path)
	return lang != "" && extraction.IsSupportedLanguage(lang)
}

// shouldQueueWatchPath reports whether an fsnotify event should enter pending.
// Remove/Rename of dirs and extensionless paths must queue even when
// DetectLanguage is empty; Create of a directory is handled separately.
func shouldQueueWatchPath(path string, op fsnotify.Op) bool {
	if op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	if IsSupported(path) {
		return true
	}
	return op&(fsnotify.Remove|fsnotify.Rename) != 0
}

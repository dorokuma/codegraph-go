package sync

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
)

func TestIsSupported(t *testing.T) {
	ok := []string{"a.go", "b.ts", "c.tsx", "d.js", "e.py", "f.rs", "g.java", "h.swift"}
	for _, f := range ok {
		if !IsSupported(f) {
			t.Errorf("expected supported: %s", f)
		}
	}
	bad := []string{"a.md", "b.css", "c.json", "d.txt"}
	for _, f := range bad {
		if IsSupported(f) {
			t.Errorf("expected unsupported: %s", f)
		}
	}
}

func TestPendingFilesEmpty(t *testing.T) {
	w := &Watcher{pending: map[string]*pendingEntry{}, workdir: "/tmp"}
	if got := w.PendingFiles(); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestStopIdempotent(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	w, err := NewWatcher(orch, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	w.Stop()
	w.Stop() // must not panic
}

func TestWatchTreeQueuesSourceFiles(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	w, err := NewWatcher(orch, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Don't Start full walk; exercise watchTree directly
	sub := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(sub, "a.go")
	if err := os.WriteFile(src, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.watchTree(sub)
	pending := w.PendingFiles()
	found := false
	for _, p := range pending {
		if filepath.Base(p) == "a.go" || p == src || filepath.ToSlash(p) == "pkg/a.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a.go queued, got %v", pending)
	}
	w.Stop()
}

// TestRescanAllQueuesSourceFiles verifies F3's rescan helper: every supported
// source file under the watch root is queued into pending (skipped dirs and
// non-source files excluded) so the debounce path reindexes them.
func TestRescanAllQueuesSourceFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.ts"), []byte("export const b = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	w, err := NewWatcher(orch, dir)
	if err != nil {
		t.Fatal(err)
	}
	w.rescanAll()

	pending := w.PendingFiles()
	got := map[string]bool{}
	for _, p := range pending {
		got[filepath.ToSlash(p)] = true
	}
	if !got["a.go"] || !got["pkg/b.ts"] {
		t.Fatalf("expected a.go and pkg/b.ts queued, got %v", pending)
	}
	if got["skip.md"] {
		t.Fatalf("non-source file must not be queued, got %v", pending)
	}
	w.Stop()
}

// TestRescanAllRegistersNewDirs verifies S1: the overflow rescan must
// (re)register directories with the fsnotify watcher. A directory created
// during the overflow window would otherwise be a permanent blind spot — its
// files get queued once, but later edits inside it never produce events.
func TestRescanAllRegistersNewDirs(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	w, err := NewWatcher(orch, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	// A directory that appears AFTER Start (e.g. created during the overflow
	// window) is not watched yet.
	newDir := filepath.Join(dir, "newpkg")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "a.go"), []byte("package newpkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.rescanAll()

	watched := map[string]bool{}
	for _, p := range w.watcher.WatchList() {
		watched[p] = true
	}
	if !watched[newDir] {
		t.Fatalf("rescanAll must register %s in the watch set, got %v", newDir, watched)
	}
	// Its source files must be queued for reindexing too.
	pending := w.PendingFiles()
	found := false
	for _, p := range pending {
		if filepath.Base(p) == "a.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected newpkg/a.go queued by rescanAll, got %v", pending)
	}
}

// TestOverflowTriggersRescan verifies F3 end to end: when fsnotify reports
// ErrEventOverflow on the Errors channel, the watcher must trigger a full
// rescan (all supported files land in pending) instead of only logging.
func TestOverflowTriggersRescan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	w, err := NewWatcher(orch, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Keep files queued long enough to observe them; the loop test only checks
	// that overflow queues a rescan, not that the reindex finishes.
	w.debounce = time.Hour
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	// Send the overflow error through the real Errors channel; the send blocks
	// until the loop receives it, so this also proves the branch is reached.
	w.watcher.Errors <- fsnotify.ErrEventOverflow

	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, p := range w.PendingFiles() {
			if p == "a.go" {
				return // overflow triggered the rescan
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("ErrEventOverflow did not trigger a full rescan (a.go never queued)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeIndexer is a scriptable indexer for exercising processPending's
// failure/requeue paths without a real extraction pipeline.
type fakeIndexer struct {
	indexChanges func(files []string) (int, int, error)
	deleteFile   func(path string) error
	deleteTree   func(path string) error
	partial      int
	failedFiles  []string // reported by IndexFailedFiles

	mu            sync.Mutex
	indexCalls    []string
	delCalls      []string
	treeCalls     []string
	recordedPanic string // set by blockedDBIndexChanges when the pass panics
}

func (f *fakeIndexer) IndexChanges(files []string) (int, int, error) {
	f.mu.Lock()
	f.indexCalls = append(f.indexCalls, files...)
	f.mu.Unlock()
	return f.indexChanges(files)
}

func (f *fakeIndexer) DeleteFile(path string) error {
	f.mu.Lock()
	f.delCalls = append(f.delCalls, path)
	f.mu.Unlock()
	return f.deleteFile(path)
}

func (f *fakeIndexer) DeleteTree(path string) error {
	f.mu.Lock()
	f.treeCalls = append(f.treeCalls, path)
	f.mu.Unlock()
	if f.deleteTree != nil {
		return f.deleteTree(path)
	}
	if f.deleteFile != nil {
		return f.deleteFile(path)
	}
	return nil
}

func (f *fakeIndexer) PartialFailures() int { return f.partial }

func (f *fakeIndexer) IndexFailedFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.failedFiles))
	copy(out, f.failedFiles)
	return out
}

func (f *fakeIndexer) gotPanic() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recordedPanic
}

func newTestWatcher(f *fakeIndexer, dir string) *Watcher {
	return &Watcher{
		orchestrator: f,
		workdir:      dir,
		pending:      map[string]*pendingEntry{},
		debounce:     0, // ready immediately
		done:         make(chan struct{}),
	}
}

// newTestWatcherNotify is newTestWatcher plus a real fsnotify handle: Stop
// closes w.watcher, so tests that call Stop need one.
func newTestWatcherNotify(f *fakeIndexer, dir string) *Watcher {
	w := newTestWatcher(f, dir)
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	w.watcher = fw
	return w
}

// blockedDBIndexChanges wires f.indexChanges to signal on started, block on
// release, and then perform a real DB query — a stand-in for the tail of a
// reindex pass that outlives Stop's grace period. The returned started
// channel lets tests wait until the pass is verifiably inside IndexChanges
// (closing done first would make processPending bail at its shutdown check
// and the test would not exercise the race at all). Any panic on the closed
// connection is recovered and recorded on the fake so tests can assert on it
// without crashing the test binary.
func blockedDBIndexChanges(f *fakeIndexer, database *db.DB, release chan struct{}) chan struct{} {
	started := make(chan struct{})
	f.indexChanges = func([]string) (int, int, error) {
		close(started)
		<-release
		var dbErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					f.mu.Lock()
					f.recordedPanic = fmt.Sprintf("panic: %v", r)
					f.mu.Unlock()
					dbErr = errors.New("db connection closed")
				}
			}()
			_, dbErr = database.GetStats()
		}()
		return 0, 0, dbErr
	}
	return started
}

// TestProcessPendingRequeuesOnIndexError: an IndexChanges error must put the
// path back into pending (it was removed at snapshot time), and repeated
// failures must be bounded by maxPendingRetries instead of retrying forever.
func TestProcessPendingRequeuesOnIndexError(t *testing.T) {
	dir := t.TempDir()
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) {
			return 0, 0, errors.New("index boom")
		},
		deleteFile: func(string) error { return nil },
	}
	w := newTestWatcher(f, dir)
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.pending[p] = &pendingEntry{since: time.Now()}

	// Attempt 1 fails → requeued, not silently dropped.
	w.processPending()
	if _, ok := w.pending[p]; !ok {
		t.Fatal("path must be requeued after IndexChanges error")
	}
	// Attempt 2 still fails → still pending (within retry budget).
	w.processPending()
	if _, ok := w.pending[p]; !ok {
		t.Fatal("path must survive until the retry cap")
	}
	// Attempt 3 reaches maxPendingRetries → dropped with a log and a
	// DroppedCount increment (retries go 0→1→2→3; 3 >= maxPendingRetries).
	w.processPending()
	if _, ok := w.pending[p]; ok {
		t.Fatal("path must be dropped once retries reach maxPendingRetries")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.indexCalls) != 3 {
		t.Fatalf("expected 3 IndexChanges calls (cap = 3 attempts), got %d", len(f.indexCalls))
	}
	if got := w.DroppedCount(); got != 1 {
		t.Fatalf("DroppedCount = %d, want 1", got)
	}
}

// TestProcessPendingRequeuesOnPartialFailure: ErrKeepOldIndex is counted as a
// partial failure and NOT returned as an error — without a requeue the change
// would be silently lost (extraction's "retry next pass" does not exist on the
// watcher path).
func TestProcessPendingRequeuesOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) { return 0, 0, nil },
		deleteFile:   func(string) error { return nil },
		partial:      1,
	}
	w := newTestWatcher(f, dir)
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.pending[p] = &pendingEntry{since: time.Now()}

	w.processPending()
	if _, ok := w.pending[p]; !ok {
		t.Fatal("partial extraction failure must requeue the path")
	}
}

// TestProcessPendingDeleteErrorRequeues: a failed DeleteFile (e.g. transient
// DB error while clearing a removed file) must be retried, not dropped.
func TestProcessPendingDeleteErrorRequeues(t *testing.T) {
	dir := t.TempDir()
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) { return 0, 0, nil },
		deleteFile:   func(string) error { return errors.New("delete boom") },
	}
	w := newTestWatcher(f, dir)
	p := filepath.Join(dir, "gone.go")
	w.pending[p] = &pendingEntry{since: time.Now()}

	w.processPending() // Lstat → IsNotExist → DeleteFile fails → requeue
	if _, ok := w.pending[p]; !ok {
		t.Fatal("DeleteFile failure must requeue the path")
	}
}

// TestProcessPendingSymlinkSkipsIndex: a path that became a symlink must be
// cleared from the index (DeleteFile), never reindexed as if it were a real
// file — extraction refuses to index symlinks, so reindexing would leave a
// ghost node/edge behind.
func TestProcessPendingSymlinkSkipsIndex(t *testing.T) {
	dir := t.TempDir()
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) { return 1, 1, nil },
		deleteFile:   func(string) error { return nil },
	}
	w := newTestWatcher(f, dir)
	real := filepath.Join(dir, "real.go")
	if err := os.WriteFile(real, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lnk := filepath.Join(dir, "link.go")
	if err := os.Symlink(real, lnk); err != nil {
		t.Fatal(err)
	}
	w.pending[lnk] = &pendingEntry{since: time.Now()}

	w.processPending()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delCalls) != 1 || f.delCalls[0] != lnk {
		t.Fatalf("expected DeleteFile(%s), got %v", lnk, f.delCalls)
	}
	for _, c := range f.indexCalls {
		if c == lnk {
			t.Fatalf("symlink path must not be reindexed: %v", f.indexCalls)
		}
	}
}

// TestProcessPendingNonRegularClearsIndex: a path that became a directory
// (file removed, dir with same name created) is not reindexable either — the
// stale index must be cleared.
func TestProcessPendingNonRegularClearsIndex(t *testing.T) {
	dir := t.TempDir()
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) { return 1, 1, nil },
		deleteFile:   func(string) error { return nil },
	}
	w := newTestWatcher(f, dir)
	p := filepath.Join(dir, "a.go")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	w.pending[p] = &pendingEntry{since: time.Now()}

	w.processPending()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.treeCalls) != 1 || f.treeCalls[0] != p {
		t.Fatalf("expected DeleteTree(%s), got tree=%v del=%v", p, f.treeCalls, f.delCalls)
	}
}

// TestProcessPendingSymlinkClearsIndex end to end: index a real file, replace
// it with a symlink, run processPending — the DB must no longer contain the
// file (no ghost nodes/edges).
func TestProcessPendingSymlinkClearsIndex(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	w := &Watcher{
		orchestrator: orch,
		workdir:      dir,
		pending:      map[string]*pendingEntry{},
		debounce:     0,
		done:         make(chan struct{}),
	}

	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package p\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := orch.IndexChanges([]string{src}); err != nil {
		t.Fatalf("index: %v", err)
	}
	files, err := database.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	indexed := false
	for _, f := range files {
		if f == "a.go" {
			indexed = true
		}
	}
	if !indexed {
		t.Fatalf("precondition failed: a.go was not indexed (files=%v)", files)
	}

	// Replace the real file with a symlink to a target outside the workspace.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent-target-outside", src); err != nil {
		t.Fatal(err)
	}

	w.pending[src] = &pendingEntry{since: time.Now()}
	w.processPending()

	files, err = database.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "a.go" {
			t.Fatalf("ghost index left after file became a symlink: %v", files)
		}
	}
}

// TestStopBoundedByGracePeriod: Stop must return within stopGracePeriod even
// when a reindex pass is stuck, instead of blocking shutdown forever.
func TestStopBoundedByGracePeriod(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) {
			<-release // block the pass until the test releases it
			return 0, 0, nil
		},
		deleteFile: func(string) error { return nil },
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	old := stopGracePeriod
	stopGracePeriod = 200 * time.Millisecond
	defer func() { stopGracePeriod = old }()

	w := &Watcher{
		orchestrator: f,
		workdir:      dir,
		watcher:      fw,
		pending:      map[string]*pendingEntry{},
		debounce:     0,
		done:         make(chan struct{}),
	}
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.pending[p] = &pendingEntry{since: time.Now()}

	w.maybeProcessPending() // starts the blocked pass off-loop
	start := time.Now()
	w.Stop()
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Stop took %v, expected to be bounded by the grace period", elapsed)
	}
	close(release) // let the in-flight pass drain so no goroutine leaks
	w.procWG.Wait()
}

// TestPendingFileStatsTruncation: the capped view must report the total queue
// size and a truncation flag, and PendingFiles must keep its old behavior.
func TestPendingFileStatsTruncation(t *testing.T) {
	w := &Watcher{pending: map[string]*pendingEntry{}, workdir: "/tmp"}
	for i := 0; i < 600; i++ {
		w.pending[fmt.Sprintf("/tmp/f%03d.go", i)] = &pendingEntry{since: time.Now()}
	}
	files, total, truncated := w.PendingFileStats()
	if total != 600 {
		t.Fatalf("total = %d, want 600", total)
	}
	if len(files) != 500 {
		t.Fatalf("len(files) = %d, want 500", len(files))
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if got := w.PendingFiles(); len(got) != 500 {
		t.Fatalf("PendingFiles len = %d, want 500", len(got))
	}
}

// TestProcessPendingRequeuesOnlyFailedPaths: a failure in a batch must
// requeue only the failing paths (per IndexFailedFiles); healthy paths
// settle and keep their retry budget untouched. The old whole-batch requeue
// burned every path's retries together, so one permanently failing file
// would drag the whole batch to the drop cap.
func TestProcessPendingRequeuesOnlyFailedPaths(t *testing.T) {
	dir := t.TempDir()
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) { return 1, 1, nil },
		deleteFile:   func(string) error { return nil },
		partial:      1,
		failedFiles:  []string{filepath.Join(dir, "bad.go")},
	}
	w := newTestWatcher(f, dir)
	bad := filepath.Join(dir, "bad.go")
	good := filepath.Join(dir, "good.go")
	for _, p := range []string{bad, good} {
		if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		w.pending[p] = &pendingEntry{since: time.Now()}
	}

	// Pass 1: bad.go fails (partial), good.go succeeds.
	w.processPending()
	if _, ok := w.pending[bad]; !ok {
		t.Fatal("bad.go must be requeued after its own failure")
	}
	if _, ok := w.pending[good]; ok {
		t.Fatal("good.go must be settled, not requeued with the failing file")
	}

	// Passes 2-3: bad.go keeps failing until its own budget is exhausted;
	// good.go must never reappear, proving its budget was never consumed by
	// bad.go's failures.
	w.processPending()
	w.processPending()
	if _, ok := w.pending[good]; ok {
		t.Fatal("good.go was requeued by bad.go's retries — budgets not isolated")
	}
	if _, ok := w.pending[bad]; ok {
		t.Fatal("bad.go must be dropped after its own retry cap")
	}
	if got := w.DroppedCount(); got != 1 {
		t.Fatalf("DroppedCount = %d, want 1 (only bad.go dropped)", got)
	}

	// The healthy file must never have been sent to IndexChanges after pass 1
	// (indexCalls is the flat list of paths: pass1 = bad+good, pass2 = bad,
	// pass3 = bad).
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.indexCalls) != 4 {
		t.Fatalf("expected 4 IndexChanges path entries (bad,good,bad,bad), got %d: %v", len(f.indexCalls), f.indexCalls)
	}
	for _, c := range f.indexCalls[2:] {
		if c == good {
			t.Fatalf("good.go was reindexed after settling: %v", f.indexCalls)
		}
	}
}

// TestStopTimeoutPassInFlightThenWaitIdlePreventsUseAfterClose is the
// regression test for the use-after-close race: Stop is bounded by
// stopGracePeriod and returns while a reindex pass is still in flight; the
// server cleanup must then wait the pass out (WaitIdle) BEFORE closing the
// DB. The pass here performs a real DB query after Stop returned, exactly
// like a single-file reindex that outlives the grace period. With WaitIdle
// before Close the query runs against an open DB (no panic, no error);
// without it the DB is closed first and the pass panics on the nil
// connection (see TestStopTimeoutCloseBeforePassDonePanics).
func TestStopTimeoutPassInFlightThenWaitIdlePreventsUseAfterClose(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	f := &fakeIndexer{deleteFile: func(string) error { return nil }}
	started := blockedDBIndexChanges(f, database, release)

	old := stopGracePeriod
	stopGracePeriod = 200 * time.Millisecond
	defer func() { stopGracePeriod = old }()

	w := newTestWatcherNotify(f, dir)
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.pending[p] = &pendingEntry{since: time.Now()}

	w.maybeProcessPending() // pass starts and blocks inside IndexChanges
	<-started               // verifiably in flight before Stop closes done

	w.Stop() // returns after the grace period with the pass still in flight

	w.procMu.Lock()
	running := w.procRunning
	w.procMu.Unlock()
	if !running {
		t.Fatal("precondition failed: the reindex pass must still be in flight after Stop's grace expired")
	}

	// The fix: WaitIdle must NOT return while the pass is blocked…
	doneCh := make(chan struct{})
	go func() {
		w.WaitIdle()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		t.Fatal("WaitIdle returned while the pass was still blocked — it must wait for in-flight work")
	case <-time.After(300 * time.Millisecond):
	}

	// …and the pass must finish while the DB is still open.
	close(release)
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitIdle did not return after the pass completed")
	}
	if got := f.gotPanic(); got != "" {
		t.Fatalf("in-flight pass panicked on the DB: %s", got)
	}
	// The pass settled the file before the DB was closed.
	w.mu.Lock()
	left := len(w.pending)
	w.mu.Unlock()
	if left != 0 {
		t.Fatalf("expected the pass to settle a.go, %d paths still pending", left)
	}
	// Cleanup ordering (the fix): Close runs only after WaitIdle.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStopTimeoutCloseBeforePassDonePanics demonstrates the race the cleanup
// fix closes: closing the DB while a pass is still in flight makes the pass
// panic on its next connection use (Close nils d.conn). The server now calls
// WaitIdle before Close so this ordering never happens in production; this
// test pins down why that ordering matters. The panic is recovered inside
// the fake indexer so it cannot crash the test binary.
func TestStopTimeoutCloseBeforePassDonePanics(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	f := &fakeIndexer{deleteFile: func(string) error { return nil }}
	started := blockedDBIndexChanges(f, database, release)

	old := stopGracePeriod
	stopGracePeriod = 200 * time.Millisecond
	defer func() { stopGracePeriod = old }()

	w := newTestWatcherNotify(f, dir)
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.pending[p] = &pendingEntry{since: time.Now()}

	w.maybeProcessPending()
	<-started // verifiably in flight before Stop closes done
	w.Stop()  // returns with the pass still in flight

	// OLD cleanup order: Close before the in-flight pass finishes.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	w.procWG.Wait() // drain the pass goroutine

	if got := f.gotPanic(); got == "" {
		t.Fatal("expected the in-flight pass to panic on the closed DB connection (this is the race WaitIdle prevents)")
	}
}

func TestShouldQueueWatchPath(t *testing.T) {
	if !shouldQueueWatchPath("pkg/a.go", fsnotify.Write) {
		t.Fatal("source write must queue")
	}
	if shouldQueueWatchPath("pkg", fsnotify.Write) {
		t.Fatal("dir write without language must not queue")
	}
	if !shouldQueueWatchPath("pkg", fsnotify.Remove) {
		t.Fatal("dir remove must queue")
	}
	if !shouldQueueWatchPath("pkg", fsnotify.Rename) {
		t.Fatal("dir rename must queue")
	}
	if shouldQueueWatchPath("README", fsnotify.Create) {
		t.Fatal("extensionless create must not queue")
	}
}

func TestProcessPendingMissingPathDeleteTree(t *testing.T) {
	dir := t.TempDir()
	f := &fakeIndexer{
		indexChanges: func([]string) (int, int, error) { return 0, 0, nil },
		deleteFile:   func(string) error { return nil },
	}
	w := newTestWatcher(f, dir)
	p := filepath.Join(dir, "pkg")
	w.pending[p] = &pendingEntry{since: time.Now()}
	w.processPending()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.treeCalls) != 1 || f.treeCalls[0] != p {
		t.Fatalf("expected DeleteTree(%s), got %v", p, f.treeCalls)
	}
}

// TestStartDegradesOnNonRootAddFailures (inotify-limit regression): a huge
// repo can exhaust fs.inotify.max_user_watches, making Watcher.Add fail for
// subdirectories. That must NOT abort Start (and with it the whole daemon —
// L4): the failure is counted and logged ("changes there will not be
// tracked") and the walk continues. Only a root Add failure aborts Start.
func TestStartDegradesOnNonRootAddFailures(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg")
	nested := filepath.Join(sub, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	addErr := errors.New("inotify limit reached (fake)")
	var mu sync.Mutex
	var added []string
	w, err := NewWatcher(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	w.addDirFn = func(path string) error {
		mu.Lock()
		defer mu.Unlock()
		added = append(added, path)
		if filepath.Clean(path) == filepath.Clean(dir) {
			return nil // the root itself is watchable
		}
		return addErr // every subtree fails, as under max_user_watches
	}

	// Capture the degradation log (the default logger is global and no test
	// in this package runs in parallel).
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	if err := w.Start(); err != nil {
		t.Fatalf("Start must degrade on non-root Add failures, got: %v", err)
	}
	w.Stop()

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{dir, sub, nested} {
		found := false
		for _, got := range added {
			if filepath.Clean(got) == filepath.Clean(want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("walk did not reach %s (added: %v); a non-root Add failure aborted the walk", want, added)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "will not be tracked") {
		t.Fatalf("degradation not logged: %q", out)
	}
	if !strings.Contains(out, sub) {
		t.Fatalf("degradation log does not name the unwatchable subtree %s: %q", sub, out)
	}
}

// TestStartFailsOnRootAddFailure: when the ROOT directory itself cannot be
// watched, nothing can be tracked — Start must fail (releasing the fsnotify
// handle, as before) instead of degrading into a no-op watcher.
func TestStartFailsOnRootAddFailure(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWatcher(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	w.addDirFn = func(string) error {
		return errors.New("inotify limit reached (fake)")
	}

	if err := w.Start(); err == nil {
		t.Fatal("root Add failure must abort Start")
	}

	// The failed Start must have released the fsnotify handle (unchanged
	// contract): a closed watcher refuses further Adds.
	if err := w.watcher.Add(dir); !errors.Is(err, fsnotify.ErrClosed) {
		t.Fatalf("failed Start must close the fsnotify watcher; Add after it returned %v", err)
	}
	w.Stop() // must not panic even after the failed Start
}

// TestStartExposesBlindSpotCounters: the degrade path exercised by
// TestStartDegradesOnNonRootAddFailures must be observable through
// BlindDirs/UnreadableDirs — the status tool reads these counters. Before
// they existed, a fully-blind watcher (inotify budget exhausted by another
// process) was visible only in the one-shot startup logs.
func TestStartExposesBlindSpotCounters(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg")
	nested := filepath.Join(sub, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	addErr := errors.New("inotify limit reached (fake)")
	w, err := NewWatcher(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	w.addDirFn = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(dir) {
			return nil // the root itself is watchable
		}
		return addErr // every subtree fails, as under max_user_watches
	}

	// Capture the summary logs (no test in this package runs in parallel).
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	if err := w.Start(); err != nil {
		t.Fatalf("Start must degrade on non-root Add failures, got: %v", err)
	}
	w.Stop()

	if got := w.BlindDirs(); got != 2 {
		t.Fatalf("BlindDirs = %d, want 2 (sub + nested unwatchable)", got)
	}
	if got := w.UnreadableDirs(); got != 0 {
		t.Fatalf("UnreadableDirs = %d, want 0", got)
	}
	if !strings.Contains(logs.String(), "will not be tracked") {
		t.Fatalf("degradation summary not logged: %q", logs.String())
	}
}

// TestStartCountsUnreadableRootWalkError: a walk error on the root (e.g. the
// workdir vanished before Start) aborts Start, but the unreadable directory
// must still be counted so the blind spot stays observable. A retrying
// caller builds a fresh Watcher per attempt, and each one reports its own
// counters.
func TestStartCountsUnreadableRootWalkError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	w, err := NewWatcher(nil, missing)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	if err := w.Start(); err == nil {
		t.Fatal("Start must fail when the workdir cannot be walked")
	}
	w.Stop() // must not panic after the failed Start

	if got := w.UnreadableDirs(); got != 1 {
		t.Fatalf("UnreadableDirs = %d, want 1", got)
	}
	if got := w.BlindDirs(); got != 0 {
		t.Fatalf("BlindDirs = %d, want 0 (the walk aborted before any Add)", got)
	}
}

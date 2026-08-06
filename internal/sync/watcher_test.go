package sync

import (
	"os"
	"path/filepath"
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
	w := &Watcher{pending: map[string]time.Time{}, workdir: "/tmp"}
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

	w := &Watcher{workdir: dir, pending: map[string]time.Time{}}
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

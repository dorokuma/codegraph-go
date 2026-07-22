package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

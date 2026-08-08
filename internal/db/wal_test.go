package db

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWALCheckpointNilDB(t *testing.T) {
	// Nil DB should return nil checkpoint (no-op).
	cp := NewWALCheckpoint(nil, time.Second)
	if cp != nil {
		t.Fatal("expected nil checkpoint for nil DB")
	}
	cp.Start() // should be no-op
	cp.Stop()  // should be no-op
}

func TestWALCheckpointZeroInterval(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	cp := NewWALCheckpoint(database, 0)
	if cp != nil {
		t.Fatal("expected nil checkpoint for zero interval")
	}
}

func TestWALCheckpointStartStop(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	cp := NewWALCheckpoint(database, 100*time.Millisecond)
	if cp == nil {
		t.Fatal("expected non-nil checkpoint")
	}
	cp.Start()
	time.Sleep(250 * time.Millisecond) // let it run a couple cycles
	cp.Stop()
	// Should not panic or hang.
}

func TestWALCheckpointIdempotentStop(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	cp := NewWALCheckpoint(database, 100*time.Millisecond)
	cp.Start()
	cp.Stop()
	cp.Stop() // second stop should be safe
}

func TestWALSize(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	// WAL size should be 0 or positive (may be 0 if checkpointed).
	size := database.WALSize()
	if size < 0 {
		t.Fatalf("WAL size should be >= 0, got %d", size)
	}
}

func TestWALInfo(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	info := database.WALInfo()
	if info == "" {
		t.Fatal("WALInfo should not be empty")
	}
}

// TestWALCheckpointCloseWithoutStop: closing the DB without stopping the
// checkpoint loop must not panic on the next ticker fire (the conn is nil).
func TestWALCheckpointCloseWithoutStop(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp := NewWALCheckpoint(database, 10*time.Millisecond)
	cp.Start()

	// Close without Stop: the next tick must see conn == nil and no-op.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // several ticks past the close
	cp.Stop()                          // must not panic
}

// TestOpenRelativeWorkdir: a relative workdir must be resolved to an absolute
// path before building the DSN (otherwise the file:// URI misparses the first
// path segment as a host) and before creating the .codegraph dir.
func TestOpenRelativeWorkdir(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	database, err := Open("proj")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if !filepath.IsAbs(database.Path()) {
		t.Fatalf("Path() must be absolute after Open, got %q", database.Path())
	}
	want := filepath.Join(dir, "proj", ".codegraph", "codegraph.db")
	if database.Path() != want {
		t.Fatalf("Path() = %q, want %q", database.Path(), want)
	}
	if _, err := os.Stat(database.Path()); err != nil {
		t.Fatalf("db file not created at absolute path: %v", err)
	}
}

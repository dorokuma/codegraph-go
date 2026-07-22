package db

import (
	"os"
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

//go:build linux

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// On Linux the DSN handed to SQLite is a /proc/self/fd/<dirfd>/codegraph.db
// magic link: it must name the pinned fd, and the fd's magic link must
// RESOLVE to the pinned directory (same dev/ino) — that is the property that
// makes SQLite's own db/-wal/-shm creations immune to a .codegraph path
// swap.
func TestPinnedDBPathUsesProcfsMagicLink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := pinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	want := fmt.Sprintf("/proc/self/fd/%d/codegraph.db", p.fd)
	if got := pinnedDBPath(p, dir); got != want {
		t.Fatalf("pinnedDBPath = %q, want %q", got, want)
	}
	procDir := fmt.Sprintf("/proc/self/fd/%d", p.fd)
	var linked, direct syscall.Stat_t
	if err := syscall.Stat(procDir, &linked); err != nil {
		t.Fatalf("magic link %q does not resolve: %v", procDir, err)
	}
	if err := syscall.Stat(dir, &direct); err != nil {
		t.Fatal(err)
	}
	if linked.Dev != direct.Dev || linked.Ino != direct.Ino {
		t.Fatalf("magic link resolves to dev=%d ino=%d, want dev=%d ino=%d",
			linked.Dev, linked.Ino, direct.Dev, direct.Ino)
	}
}

// A nil pin and a degraded pin (no fd — pinnedDBPath's procfs pre-flight
// falls back when the magic link does not resolve) must never produce a
// /proc/self/fd path at all: both fall back to the validated plain path.
func TestPinnedDBPathFallsBackToPlainPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "codegraph.db")
	if got := pinnedDBPath(nil, dir); got != want {
		t.Fatalf("pinnedDBPath(nil) = %q, want %q", got, want)
	}
	if got := pinnedDBPath(&pinnedDir{fd: -1}, dir); got != want {
		t.Fatalf("pinnedDBPath(degraded) = %q, want %q", got, want)
	}
}

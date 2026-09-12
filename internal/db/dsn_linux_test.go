//go:build linux

package db

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// On Linux the DSN handed to SQLite is a /proc/self/fd/<dirfd>/codegraph.db
// magic link: it must name the pinned fd, and the fd's magic link must
// RESOLVE to the pinned directory (same dev/ino) - that is the property that
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
	got, gerr := pinnedDBPath(p, dir)
	if gerr != nil {
		t.Fatalf("pinnedDBPath: %v", gerr)
	}
	if got != want {
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

// A nil pin and a degraded pin (no fd) have no magic link to probe, so both
// keep receiving the validated plain path, with no error.
func TestPinnedDBPathFallsBackToPlainPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "codegraph.db")
	got, gerr := pinnedDBPath(nil, dir)
	if gerr != nil || got != want {
		t.Fatalf("pinnedDBPath(nil) = %q, %v, want %q", got, gerr, want)
	}
	got, gerr = pinnedDBPath(&pinnedDir{fd: -1}, dir)
	if gerr != nil || got != want {
		t.Fatalf("pinnedDBPath(degraded) = %q, %v, want %q", got, gerr, want)
	}
}

// injectProcFdStat swaps the procfs probe used by pinnedDBPath and returns
// its restoration.
func injectProcFdStat(fn func(string) (os.FileInfo, error)) (restore func()) {
	old := procFdStatFn
	procFdStatFn = fn
	return func() { procFdStatFn = old }
}

// procfs probe failures must fail closed by default: on the plain path the
// TOCTOU defense degrades from prevention to detection only, so Open gets an
// explicit, actionable error (naming the escape hatch) instead.
func TestPinnedDBPathFailsClosedWhenProcfsUnavailable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := pinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	restore := injectProcFdStat(func(string) (os.FileInfo, error) {
		return nil, errors.New("procfs unavailable")
	})
	defer restore()

	got, gerr := pinnedDBPath(p, dir)
	if got != "" || gerr == nil {
		t.Fatalf("pinnedDBPath = %q, err=%v; want fail-closed error", got, gerr)
	}
	if !strings.Contains(gerr.Error(), AllowPlainDBEnv) {
		t.Fatalf("error must name the escape hatch %s, got: %v", AllowPlainDBEnv, gerr)
	}
}

// A magic link that resolves to something other than a directory is equally
// unusable: fail closed by default as well.
func TestPinnedDBPathFailsClosedWhenMagicLinkNotADir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := pinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	plainFile := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(plainFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := injectProcFdStat(func(string) (os.FileInfo, error) { return os.Stat(plainFile) })
	defer restore()

	got, gerr := pinnedDBPath(p, dir)
	if got != "" || gerr == nil {
		t.Fatalf("pinnedDBPath = %q, err=%v; want fail-closed error", got, gerr)
	}
}

// Escape hatch: with CODEGRAPH_ALLOW_PLAIN_DB=1 the historical fallback is
// kept (with a logged warning), and the degraded plain-path DSN still has
// the recheck layer of pin_unix.go as its detection line - after a swap of
// the .codegraph path, recheckDir must still detect it.
func TestPinnedDBPathEscapeHatchFallsBackAndRechecksStillFire(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := pinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	t.Setenv(AllowPlainDBEnv, "1")
	restore := injectProcFdStat(func(string) (os.FileInfo, error) {
		return nil, errors.New("procfs unavailable")
	})
	defer restore()

	plain := filepath.Join(dir, "codegraph.db")
	got, gerr := pinnedDBPath(p, dir)
	if gerr != nil || got != plain {
		t.Fatalf("pinnedDBPath = %q, err=%v; want plain-path fallback %q", got, gerr, plain)
	}

	// The degraded DSN's remaining defense: swap .codegraph for a symlink
	// and the recheck lines must still detect the swap.
	evil := filepath.Join(t.TempDir(), "evil")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, dir); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	if err := p.recheckDir(dir); err == nil {
		t.Fatal("recheckDir must detect the swap on the degraded fallback path")
	}
}

// End to end: Open must propagate the fail-closed error when procfs is
// unavailable and the escape hatch is not set (flock and pin are released on
// the error path).
func TestOpenFailsClosedWhenProcfsUnavailable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	restore := injectProcFdStat(func(string) (os.FileInfo, error) {
		return nil, errors.New("procfs unavailable")
	})
	defer restore()

	db, err := Open(root)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open must fail closed when procfs is unavailable")
	}
	if !strings.Contains(err.Error(), AllowPlainDBEnv) {
		t.Fatalf("Open error must name the escape hatch, got: %v", err)
	}
}

// End to end escape hatch: with CODEGRAPH_ALLOW_PLAIN_DB=1 Open succeeds
// through the plain path and the database is usable.
func TestOpenEscapeHatchUsesPlainPath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(AllowPlainDBEnv, "1")
	restore := injectProcFdStat(func(string) (os.FileInfo, error) {
		return nil, errors.New("procfs unavailable")
	})
	defer restore()

	db, err := Open(root)
	if err != nil {
		t.Fatalf("Open with %s=1 must fall back, got: %v", AllowPlainDBEnv, err)
	}
	if want := filepath.Join(root, ".codegraph", "codegraph.db"); db.Path() != want {
		t.Fatalf("db path = %q, want %q", db.Path(), want)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

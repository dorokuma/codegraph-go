//go:build unix

package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the dirfd pinning that closes the post-validation TOCTOU in
// db.Open: the pinned fd's fstat is compared against a fresh stat of the
// path, and a mismatch — the .codegraph path swapped after validation — must
// abort. statPathFn is the injection point that makes the swap
// deterministic; without it these races could only be won by polling.

func TestPinnedDirMatchesUnswappedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := pinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()
	if err := p.recheckDir(dir); err != nil {
		t.Fatalf("a pinned, unmodified directory must pass the recheck: %v", err)
	}
}

func TestPinnedDirDetectsSymlinkSwap(t *testing.T) {
	// The red-team swap, made deterministic: pin the real directory, then
	// replace the .codegraph path with a symlink to an outside directory.
	// The pinned fd still refers to the real directory inode, so the recheck
	// must see the dev/ino mismatch and refuse.
	proj := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj, ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := pinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	defer func() { _ = os.Remove(dir) }()

	err = p.recheckDir(dir)
	if err == nil {
		t.Fatal("a .codegraph swapped to a symlink after pinning must fail the recheck")
	}
	if !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("recheck error should name the post-validation swap, got: %v", err)
	}
}

func TestRecheckChildInodeComparisons(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	dbA := filepath.Join(dirA, "codegraph.db")
	dbB := filepath.Join(dirB, "codegraph.db")
	for _, p := range []string{dbA, dbB} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pinned, err := pinDir(dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pinned.close() }()

	orig := statPathFn
	defer func() { statPathFn = orig }()

	// Path stat resolves to the same file the pinned fd sees: pass.
	statPathFn = func(string) (os.FileInfo, error) { return os.Stat(dbA) }
	if err := pinned.recheckChild(dirA, "codegraph.db", dbA); err != nil {
		t.Fatalf("matching dev/ino must pass the recheck: %v", err)
	}

	// Path stat resolves to a DIFFERENT file (two distinct directory
	// constructs, hence distinct dev/ino): the db escaped the pinned
	// directory, refuse.
	statPathFn = func(string) (os.FileInfo, error) { return os.Stat(dbB) }
	err = pinned.recheckChild(dirA, "codegraph.db", dbA)
	if err == nil {
		t.Fatal("mismatched dev/ino must fail the recheck")
	}
	if !strings.Contains(err.Error(), "escaped the pinned .codegraph dir") {
		t.Fatalf("recheck error should name the escape, got: %v", err)
	}
}

func TestOpenAbortsWhenDirSwappedAfterValidation(t *testing.T) {
	// Full-Open TOCTOU replay: the first path stat inside Open (the lock
	// recheck) performs the red-team swap — real directory away, symlink to
	// the victim in — then stats the path. Open must abort on the dev/ino
	// mismatch and leave the victim untouched (the lock file was created
	// through the pinned fd, i.e. into the renamed real directory).
	proj := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj, ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	orig := statPathFn
	defer func() { statPathFn = orig }()
	swapped := false
	statPathFn = func(path string) (os.FileInfo, error) {
		if !swapped && path == dir {
			swapped = true
			if err := os.Rename(dir, dir+".real"); err != nil {
				return nil, err
			}
			if err := os.Symlink(victim, dir); err != nil {
				return nil, err
			}
		}
		return os.Stat(path)
	}

	_, err := Open(proj)
	if err == nil {
		t.Fatal("Open must abort when the .codegraph path is swapped after validation")
	}
	if !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("error should name the post-validation swap, got: %v", err)
	}
	entries, rerr := os.ReadDir(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target polluted, want zero entries, got: %v", entries)
	}
	_ = os.Remove(dir)
}

func TestOpenDetectsDbEscapeAfterWAL(t *testing.T) {
	// Second line: the swap happens later — while the post-WAL db recheck
	// stats dbPath. The victim holds a pre-existing dummy codegraph.db so the
	// path stat resolves and the INODES (not the file's existence) decide:
	// mismatch means the database escaped the pinned directory. The victim
	// must gain no new entries (no -wal, -shm, lock).
	proj := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "codegraph.db"), []byte("not a db"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj, ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	orig := statPathFn
	defer func() { statPathFn = orig }()
	dbPath := filepath.Join(dir, "codegraph.db")
	swapped := false
	statPathFn = func(path string) (os.FileInfo, error) {
		if !swapped && path == dbPath {
			swapped = true
			if err := os.Rename(dir, dir+".real"); err != nil {
				return nil, err
			}
			if err := os.Symlink(victim, dir); err != nil {
				return nil, err
			}
		}
		return os.Stat(path)
	}

	_, err := Open(proj)
	if err == nil {
		t.Fatal("Open must detect codegraph.db escaping the pinned directory")
	}
	if !strings.Contains(err.Error(), "escaped the pinned .codegraph dir") {
		t.Fatalf("error should name the escape, got: %v", err)
	}
	entries, rerr := os.ReadDir(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 1 || entries[0].Name() != "codegraph.db" {
		t.Fatalf("victim must hold only the pre-existing dummy, got: %v", entries)
	}
	_ = os.Remove(dir)
}

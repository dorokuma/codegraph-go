package cgdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The .codegraph symlink jail: a symlinked .codegraph makes every create or
// write under it land at the symlink TARGET — outside the project. These
// tests pin the guard on both defense lines (Lstat, realpath) and on the
// combined Ensure used by db.Open and every daemon write point.

// newSymlinkedProject builds a project whose .codegraph is a symlink pointing
// at an outside victim directory.
func newSymlinkedProject(t *testing.T) (proj, victim string) {
	t.Helper()
	proj = t.TempDir()
	outside := t.TempDir()
	victim = filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(proj, ".codegraph")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	return proj, victim
}

// assertTargetClean fails when the symlink victim directory gained entries.
func assertTargetClean(t *testing.T, victim string) {
	t.Helper()
	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target polluted, want zero entries, got: %v", entries)
	}
}

func TestEnsureRejectsSymlinkCodeGraph(t *testing.T) {
	proj, victim := newSymlinkedProject(t)
	dir := filepath.Join(proj, ".codegraph")

	if err := Ensure(dir); err == nil {
		t.Fatal("Ensure must refuse a symlinked .codegraph")
	} else if !strings.Contains(err.Error(), ".codegraph is a symlink; refusing to open") {
		t.Fatalf("error must name the symlink refusal, got: %v", err)
	}
	assertTargetClean(t, victim)
}

func TestEnsureCreatesMissingDirAndKeepsRealDir(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".codegraph")

	// Missing: Ensure creates a fresh real directory.
	if err := Ensure(dir); err != nil {
		t.Fatalf("Ensure must create a missing .codegraph: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf(".codegraph must be a real directory after Ensure, got %v", fi.Mode())
	}

	// Real: Ensure passes and is idempotent.
	if err := Ensure(dir); err != nil {
		t.Fatalf("Ensure must accept an existing real directory: %v", err)
	}

	// Not a directory: refused.
	other := filepath.Join(proj, "notadir")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(other); err == nil {
		t.Fatal("Ensure must refuse a non-directory")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error should say .codegraph is not a directory, got: %v", err)
	}
}

func TestEnsureSymlinkedAncestorPasses(t *testing.T) {
	// A real .codegraph reached through a symlinked ANCESTOR must pass:
	// only the .codegraph component itself may not alias (symlinked workdirs
	// are a supported alias of their real path).
	realRoot := filepath.Join(t.TempDir(), "realproj")
	if err := os.MkdirAll(filepath.Join(realRoot, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := Ensure(filepath.Join(alias, ".codegraph")); err != nil {
		t.Fatalf("symlinked ancestor must not fail Ensure: %v", err)
	}
}

func TestDefenseLines(t *testing.T) {
	proj, victim := newSymlinkedProject(t)
	dir := filepath.Join(proj, ".codegraph")

	// Line 1 (Lstat): a symlinked .codegraph is rejected up front.
	if err := rejectSymlinkDir(dir); err == nil {
		t.Fatal("Lstat line must reject a symlinked .codegraph")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Lstat line error should mention symlink: %v", err)
	}

	// Line 1 pass cases: missing (created later) and a real directory.
	missing := filepath.Join(t.TempDir(), ".codegraph")
	if err := rejectSymlinkDir(missing); err != nil {
		t.Fatalf("missing .codegraph must pass the Lstat line: %v", err)
	}
	real := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rejectSymlinkDir(real); err != nil {
		t.Fatalf("real .codegraph must pass the Lstat line: %v", err)
	}

	// Line 2 (realpath): a .codegraph that resolves elsewhere is refused.
	if err := verifyRealPath(dir); err == nil {
		t.Fatal("realpath line must refuse a .codegraph that resolves elsewhere")
	} else if !strings.Contains(err.Error(), ".codegraph is a symlink; refusing to open") {
		t.Fatalf("realpath line error should name the symlink refusal: %v", err)
	}
	if err := verifyRealPath(real); err != nil {
		t.Fatalf("real .codegraph must pass the realpath line: %v", err)
	}

	// A symlinked ANCESTOR must not fail the realpath line (see Ensure).
	realRoot := filepath.Join(t.TempDir(), "realproj")
	if err := os.MkdirAll(filepath.Join(realRoot, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := verifyRealPath(filepath.Join(alias, ".codegraph")); err != nil {
		t.Fatalf("symlinked ancestor must not fail the realpath line: %v", err)
	}

	assertTargetClean(t, victim)
}

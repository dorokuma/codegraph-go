package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The .codegraph symlink jail (adversarial audit): a symlinked .codegraph used
// to pass walk-up detection (os.Stat follows the link), and db.Open then
// MkdirAll-ed along it, so codegraph.db, -wal, -shm and codegraph.lock landed
// at the symlink TARGET - outside the project. These tests pin the fix on
// both sides: walk-up must skip a symlinked .codegraph, Open must refuse it,
// and the jail must hold with zero files created at the symlink target. (The
// two defense lines themselves — Lstat and realpath, shared as cgdir.Ensure
// — are tested white-box in internal/cgdir.)

// newSymlinkedProject builds a project whose .codegraph is a symlink pointing
// at an outside victim directory, plus a src/ subdirectory to walk up from.
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
	if err := os.MkdirAll(filepath.Join(proj, "src"), 0o700); err != nil {
		t.Fatal(err)
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

func TestOpenRejectsSymlinkCodeGraph(t *testing.T) {
	proj, victim := newSymlinkedProject(t)

	_, err := Open(proj)
	if err == nil {
		t.Fatal("Open must refuse a symlinked .codegraph")
	}
	if !strings.Contains(err.Error(), ".codegraph is a symlink; refusing to open") {
		t.Fatalf("error must name the symlink refusal, got: %v", err)
	}
	assertTargetClean(t, victim)
}

func TestOpenRejectsFileCodeGraph(t *testing.T) {
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, ".codegraph"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(proj)
	if err == nil {
		t.Fatal("Open must refuse a .codegraph that is not a directory")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error should say .codegraph is not a directory, got: %v", err)
	}
}

func TestWalkUpSkipsSymlinkCodeGraph(t *testing.T) {
	proj, victim := newSymlinkedProject(t)

	if IsIndexed(proj) {
		t.Fatal("symlinked .codegraph must not count as indexed")
	}
	if got := FindNearestCodeGraphRoot(filepath.Join(proj, "src")); got != "" {
		t.Fatalf("walk-up must skip symlinked .codegraph, got root %q", got)
	}
	assertTargetClean(t, victim)
}

func TestWalkUpClimbsPastSymlinkToRealRoot(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	// Real index at base; the child project only has a symlinked .codegraph.
	if err := os.MkdirAll(filepath.Join(base, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(proj, ".codegraph")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	if got := FindNearestCodeGraphRoot(filepath.Join(proj, "src")); got != base {
		t.Fatalf("walk-up must climb past the symlinked .codegraph to the real root %q, got %q", base, got)
	}
	assertTargetClean(t, victim)
}

func TestRealCodeGraphBehaviorUnchanged(t *testing.T) {
	proj := t.TempDir()
	if IsIndexed(proj) {
		t.Fatal("fresh dir without .codegraph must not be indexed")
	}

	database, err := Open(proj)
	if err != nil {
		t.Fatalf("Open must create and use a real .codegraph: %v", err)
	}
	want := filepath.Join(proj, ".codegraph", "codegraph.db")
	if database.Path() != want {
		t.Fatalf("db path = %q, want %q", database.Path(), want)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if !IsIndexed(proj) {
		t.Fatal("real .codegraph must count as indexed")
	}
	if got := FindNearestCodeGraphRoot(filepath.Join(proj, "anything")); got != proj {
		t.Fatalf("walk-up = %q, want %q", got, proj)
	}
}

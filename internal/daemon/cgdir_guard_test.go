package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Daemon-side .codegraph symlink jail regression: before the guard, the three
// daemon write points (SpawnDetached's daemon.log, TryAcquireLock's pidfile,
// Start's socket bind) all bypassed the .codegraph defense lines and wrote
// straight through a symlinked .codegraph to its target — the red team
// demonstrated an out-of-jail daemon.log. Each write point must now run
// cgdir.Ensure first and fail closed with zero files created at the target.

// newSymlinkedProject builds a project whose .codegraph is a symlink pointing
// at an outside victim directory (same fixture style as db's
// symlink_guard_test.go).
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

func assertSymlinkRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("must refuse a symlinked .codegraph")
	}
	if !strings.Contains(err.Error(), ".codegraph is a symlink; refusing to open") {
		t.Fatalf("error must name the symlink refusal, got: %v", err)
	}
}

func TestSpawnDetachedRejectsSymlinkCodeGraph(t *testing.T) {
	proj, victim := newSymlinkedProject(t)

	// Ensure fails before resolveDaemonBinary, so no daemon process is
	// spawned and no daemon.log is created — at the target or anywhere.
	assertSymlinkRefusal(t, SpawnDetached(proj, nil))
	assertTargetClean(t, victim)
}

func TestTryAcquireLockRejectsSymlinkCodeGraph(t *testing.T) {
	proj, victim := newSymlinkedProject(t)

	_, err := TryAcquireLock(proj)
	assertSymlinkRefusal(t, err)
	assertTargetClean(t, victim)
}

func TestDaemonStartRejectsSymlinkCodeGraph(t *testing.T) {
	proj, victim := newSymlinkedProject(t)

	// Start guards .codegraph before bind(2) — and before the os.Remove of a
	// stale socket, which would otherwise delete through the symlink.
	d := New(proj, func(ctx context.Context, rwc io.ReadWriteCloser) error {
		return nil
	})
	assertSymlinkRefusal(t, d.Start())
	assertTargetClean(t, victim)
}

func TestTryAcquireLockCreatesMissingCodeGraphDir(t *testing.T) {
	// The guard must not over-reject: a fresh project without .codegraph
	// passes (Ensure creates the real directory) and the lock is acquired.
	root := t.TempDir()
	res, err := TryAcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "acquired" {
		t.Fatalf("want acquired, got %+v", res)
	}
	fi, err := os.Lstat(CodeGraphDir(root))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf(".codegraph must be a real directory after TryAcquireLock, got %v", fi.Mode())
	}
	defer func() { _ = os.Remove(res.PidPath) }()
}

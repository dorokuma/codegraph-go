package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
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

// TestSpawnDetachedDaemonLogOpenFailureFailsClosed is the RED regression for
// the swallowed daemon.log open failure (diagnostic-fix rounds 1-2): the
// baseline sets logFile=nil and spawns anyway when daemon.log cannot be
// opened — the child runs with stdout/stderr discarded and the parent only
// sees a dial timeout (CG_B5_EISDIR_BLIND_SPAWN). A directory planted at the
// daemon.log path is a root-proof EISDIR trigger (chmod would be skipped
// under euid 0); the same merge absorbs the duplicate EISDIR sub-case that
// used to live in spawn_test.go, keeping the independent non-root EACCES
// leg there. SpawnDetached must fail closed BEFORE spawning anything: the
// error must carry the EISDIR errno, the target root and the log path; the
// systemdRunArgsFn counter proves startup preparation was never reached and
// the marker proves no child ever ran.
//
// Baseline-injection note: this test deliberately uses only symbols that
// exist at ae3dd51 (SpawnDetached, CodeGraphDir, resolveDaemonBinaryFn,
// systemdRunArgsFn), so copying this file onto a bare ae3dd51 clone
// compiles and FAILS substantively (err == nil, child spawned) — not with
// undefined symbols.
func TestSpawnDetachedDaemonLogOpenFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	if err := os.MkdirAll(logPath, 0o755); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "spawned.marker")
	self := filepath.Join(root, "fake-daemon.sh")
	body := "#!/bin/sh\nprintf ran > '" + marker + "'\n"
	if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	oldResolve := resolveDaemonBinaryFn
	resolveDaemonBinaryFn = func() (string, error) { return self, nil }
	defer func() { resolveDaemonBinaryFn = oldResolve }()
	// Startup-preparation probe: systemdRunArgsFn is only evaluated once the
	// log was opened successfully, so a nonzero count means the spawn path
	// continued past the refused open.
	var prepCalls atomic.Int64
	oldArgs := systemdRunArgsFn
	systemdRunArgsFn = func(string, []string, *SpawnOpts) []string {
		prepCalls.Add(1)
		return nil
	}
	defer func() { systemdRunArgsFn = oldArgs }()

	err := SpawnDetached(root, nil)
	if err == nil {
		t.Fatal("CG_B5_EISDIR_BLIND_SPAWN an unopenable daemon.log must fail SpawnDetached closed, not spawn a child whose output is discarded")
	}
	if !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("the error must carry the open failure errno EISDIR, got: %v", err)
	}
	if !strings.Contains(err.Error(), logPath) || !strings.Contains(err.Error(), root) {
		t.Fatalf("the error must name the daemon log path and the target root, got: %v", err)
	}
	if got := prepCalls.Load(); got != 0 {
		t.Fatalf("CG_B5_EISDIR_BLIND_SPAWN reached startup preparation %d time(s) after the open failure", got)
	}
	// Bound the wait so a baseline build (which DID spawn the fake) cannot
	// race the marker write, then assert the fix never spawned at all.
	time.Sleep(300 * time.Millisecond)
	if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
		t.Fatalf("CG_B5_EISDIR_BLIND_SPAWN a child was spawned despite the log open failure (marker present: %v)", serr)
	}
}

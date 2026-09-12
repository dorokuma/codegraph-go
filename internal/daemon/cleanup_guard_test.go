package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The cleanup-side red-team scenario (0.9.9): a daemon is RUNNING when its
// .codegraph directory is swapped for a symlink to an attacker directory
// that holds planted daemon.pid/daemon.sock. On SIGTERM the old
// cleanupArtifacts removed through the (now swapped) paths and deleted the
// planted files. The recorded dir identity must make those removals skip.

// newSwappedDaemonRoot builds a project whose .codegraph is a symlink to an
// outside "evil" directory holding planted artifacts (a pidfile naming US,
// so the historical content check alone would have passed), plus the
// renamed real directory. It returns the identity of the real directory as
// it was BEFORE the swap — what Start records right after cgdir.Ensure.
func newSwappedDaemonRoot(t *testing.T) (root, evil, realDir string, preID *dirIdentity) {
	t.Helper()
	root = t.TempDir()
	cg := CodeGraphDir(root)
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}

	preID, err := statDirIdentity(cg)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	evil = filepath.Join(outside, "evil")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := EncodeLock(LockInfo{PID: os.Getpid(), Version: PackageVersion})
	if err := os.WriteFile(filepath.Join(evil, "daemon.pid"), planted, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "daemon.sock"), []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(cg, cg+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, cg); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })
	return root, evil, cg + ".real", preID
}

// mustExist fails when path does not exist (a planted artifact was removed
// through the swapped path, or a real artifact disappeared unexpectedly).
func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s must survive the guarded cleanup: %v", path, err)
	}
}

// mustNotExist fails when path still exists (a removal was skipped although
// the identity matched).
func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s must be removed by the guarded cleanup: %v", path, err)
	}
}

// TestCleanupArtifactsSkipsRemovalAfterDirSwap: with the .codegraph path
// swapped while the daemon holds the pre-swap identity, the planted
// artifacts in the symlink target must not be removed.
func TestCleanupArtifactsSkipsRemovalAfterDirSwap(t *testing.T) {
	root, evil, _, preID := newSwappedDaemonRoot(t)

	d := New(root, func(context.Context, io.ReadWriteCloser) error { return nil })
	d.dirID = preID
	d.socketPath = filepath.Join(CodeGraphDir(root), "daemon.sock") // resolves through the symlink now

	d.cleanupArtifacts()

	mustExist(t, filepath.Join(evil, "daemon.pid"))
	mustExist(t, filepath.Join(evil, "daemon.sock"))
}

// TestCleanupArtifactsRemovesWhenIdentityMatches (control): an unswapped
// .codegraph still gets its pidfile and socket removed by Stop's cleanup.
func TestCleanupArtifactsRemovesWhenIdentityMatches(t *testing.T) {
	root := t.TempDir()
	cg := CodeGraphDir(root)
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}

	d := New(root, func(context.Context, io.ReadWriteCloser) error { return nil })
	id, err := statDirIdentity(cg)
	if err != nil {
		t.Fatal(err)
	}
	d.dirID = id
	if err := os.WriteFile(d.pidPath, EncodeLock(LockInfo{PID: os.Getpid(), Version: PackageVersion}), 0o600); err != nil {
		t.Fatal(err)
	}
	d.socketPath = filepath.Join(cg, "daemon.sock")
	if err := os.WriteFile(d.socketPath, []byte("socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	d.cleanupArtifacts()

	mustNotExist(t, d.pidPath)
	mustNotExist(t, d.socketPath)
}

// TestStopAfterDirSwapLeavesSymlinkTargetClean runs the full lifecycle:
// Start records the identity, the directory is swapped while the daemon
// serves, Stop (the SIGTERM path) must skip every .codegraph removal — the
// planted files in the symlink target survive, including the socket (the
// runtime's unlink-on-close is disabled so the guarded cleanup is the only
// socket removal).
func TestStopAfterDirSwapLeavesSymlinkTargetClean(t *testing.T) {
	root := t.TempDir()
	cg := CodeGraphDir(root)
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}

	d := New(root, func(context.Context, io.ReadWriteCloser) error { return nil })
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}

	// Swap while "running": real dir away, symlink to evil in, artifacts
	// planted (pidfile names us, so the content check alone would pass).
	outside := t.TempDir()
	evil := filepath.Join(outside, "evil")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := EncodeLock(LockInfo{PID: os.Getpid(), Version: PackageVersion})
	if err := os.WriteFile(filepath.Join(evil, "daemon.pid"), planted, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "daemon.sock"), []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cg, cg+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, cg); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })

	d.Stop("test end") // the SIGTERM / idle-exit path

	mustExist(t, filepath.Join(evil, "daemon.pid"))
	mustExist(t, filepath.Join(evil, "daemon.sock"))
	// The daemon's own artifacts in the renamed real directory are left for
	// the next start's stale-lock handling rather than deleted through the
	// swapped path.
	if d.socketPath != filepath.Join(cg, "daemon.sock") {
		t.Fatalf("socket path = %q", d.socketPath)
	}
	mustExist(t, filepath.Join(cg+".real", "daemon.pid"))
	mustExist(t, filepath.Join(cg+".real", "daemon.sock"))
}

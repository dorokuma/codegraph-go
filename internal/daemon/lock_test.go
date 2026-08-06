package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTryAcquireLockExclusive(t *testing.T) {
	root := t.TempDir()
	// Ensure .codegraph exists
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}

	a, err := TryAcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "acquired" {
		t.Fatalf("first acquire: %+v", a)
	}

	b, err := TryAcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != "taken" {
		t.Fatalf("second acquire want taken, got %+v", b)
	}
	if b.Existing == nil || b.Existing.PID != os.Getpid() {
		t.Fatalf("existing lock %+v", b.Existing)
	}

	// Clear with wrong expected pid → refuse
	if ClearStaleLock(a.PidPath, os.Getpid()+999) {
		// may still clear if decode fails; with live pid should refuse
		if !IsProcessAlive(os.Getpid()) {
			t.Fatal("self not alive?")
		}
	}
	// Live holder: ClearStaleLock must not delete
	if ClearStaleLock(a.PidPath, os.Getpid()) {
		t.Fatal("cleared live lock")
	}
	if _, err := os.Stat(a.PidPath); err != nil {
		t.Fatal("lockfile missing after refused clear")
	}

	// Simulate dead pid by rewriting lock to a likely-dead pid
	dead := LockInfo{PID: 1 << 30, Version: PackageVersion, SocketPath: PreferredSocket(root), StartedAt: 1}
	if err := os.WriteFile(a.PidPath, EncodeLock(dead), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsProcessAlive(dead.PID) {
		t.Skip("unlikely pid is alive on this host")
	}
	if !ClearStaleLock(a.PidPath, dead.PID) {
		t.Fatal("failed to clear stale lock")
	}
	if _, err := os.Stat(a.PidPath); !os.IsNotExist(err) {
		t.Fatal("stale lock still present")
	}

	// Re-acquire works
	c, err := TryAcquireLock(root)
	if err != nil || c.Kind != "acquired" {
		t.Fatalf("reacquire %+v err=%v", c, err)
	}
	_ = os.Remove(c.PidPath)
}

func TestIsProcessAliveSelf(t *testing.T) {
	if !IsProcessAlive(os.Getpid()) {
		t.Fatal("self should be alive")
	}
	if IsProcessAlive(0) || IsProcessAlive(-1) {
		t.Fatal("invalid pids")
	}
}

func TestKillStaleDaemonDeadPID(t *testing.T) {
	// B1: KillStaleDaemon with a pidfile naming a dead process must clear the
	// stale lock and return nil (no signal needed).
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	dead := LockInfo{PID: 1 << 30, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1}
	if err := os.WriteFile(pidPath, EncodeLock(dead), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsProcessAlive(dead.PID) {
		t.Skip("unlikely pid is alive on this host")
	}
	if err := KillStaleDaemon(root); err != nil {
		t.Fatalf("KillStaleDaemon: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("stale pidfile not cleared")
	}
}

func TestKillStaleDaemonSignalsLiveProcess(t *testing.T) {
	// B1: KillStaleDaemon SIGTERMs the live process named in the pidfile,
	// waits for it to exit, then clears the lock.
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	// Reap the child in the background — like a real daemon (reparented via
	// SpawnDetached's Process.Release), the helper must not linger as a
	// zombie or IsProcessAlive would see it as still alive.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer cmd.Process.Kill() //nolint:errcheck
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := KillStaleDaemon(root); err != nil {
		t.Fatalf("KillStaleDaemon: %v", err)
	}
	// The helper must have been terminated and the lock cleared.
	select {
	case werr := <-waited:
		if werr == nil {
			t.Fatal("helper process still running after KillStaleDaemon")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper process not terminated after KillStaleDaemon")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("stale pidfile not cleared after kill")
	}
}

func TestRegistryRoundtrip(t *testing.T) {
	// Use a unique root so we don't clobber real registry entries.
	root := filepath.Join(t.TempDir(), "proj")
	rec := Record{Root: root, PID: os.Getpid(), Version: "0.7.0", SocketPath: "/tmp/x.sock", StartedAt: 1}
	Register(rec)
	defer Deregister(root)
	list := List()
	found := false
	for _, r := range list {
		if r.Root == root && r.PID == os.Getpid() {
			found = true
		}
	}
	if !found {
		t.Fatalf("record not in list: %+v", list)
	}
	Deregister(root)
}

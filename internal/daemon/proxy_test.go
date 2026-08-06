package daemon

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEnsureAndDialRefusesStaleKill: when the version-mismatch cleanup hits a
// live process whose identity cannot be confirmed (PID-reuse guard), the
// stale-daemon kill is REFUSED. EnsureAndDial must NOT spawn a replacement
// (the lock is still held by an unidentified process — spawning would risk
// double-writing the index) and must hand the refusal to the caller as an
// error wrapping ErrStaleDaemonRefused (G1).
func TestEnsureAndDialRefusesStaleKill(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}

	// A live, unrelated process owns the recorded pid: ProcStart=1 can never
	// match a real process started after boot, so identity verification must
	// refuse (start time changed, or unreadable on non-procfs platforms —
	// either way it is a refusal).
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	pidPath := PidPath(root)
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: 1}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	// A socket that speaks a different build version: DialAnyCandidate must
	// report a definitive version mismatch so EnsureAndDial enters the
	// stale-daemon kill path.
	sock := PreferredSocket(root)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(`{"codegraph":"9.9.9","pid":1,"socketPath":"x","protocol":1}` + "\n"))
	}()

	conn, br, hello, ok, err := EnsureAndDial(root, 200*time.Millisecond, 10*time.Millisecond, nil)
	if ok {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("EnsureAndDial succeeded despite identity refusal")
	}
	if err == nil || !errors.Is(err, ErrStaleDaemonRefused) {
		t.Fatalf("expected ErrStaleDaemonRefused, got ok=%v err=%v", ok, err)
	}
	if conn != nil || br != nil || hello.PID != 0 {
		t.Fatalf("unexpected non-nil outputs: conn=%v br=%v hello=%+v", conn, br, hello)
	}
	// The refusal reason must name the pidfile so the caller can point the
	// user at it.
	if !strings.Contains(err.Error(), pidPath) {
		t.Fatalf("refusal error does not mention pidfile %s: %v", pidPath, err)
	}

	// The unidentified process must still be running (never signaled) and the
	// lock still present (never cleared).
	select {
	case <-done:
		t.Fatal("helper process was killed despite identity refusal")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite identity refusal: %v", serr)
	}

	// No daemon was spawned: SpawnDetached would have created daemon.log in
	// .codegraph/ (it opens the log before starting the child).
	if _, serr := os.Stat(filepath.Join(CodeGraphDir(root), "daemon.log")); !os.IsNotExist(serr) {
		t.Fatalf("a daemon was spawned despite identity refusal (daemon.log present: %v)", serr)
	}

	_ = cmd.Process.Kill()
	<-done
}

// TestHalfDeadDaemonHoldsLockStartupGrace (M2 regression): half-dead
// classification must respect a startup window — a pidfile written less than
// halfDeadStartupGrace ago names a healthy daemon that is still binding its
// socket (TryAcquireLock writes the pidfile before Daemon.Start binds), so it
// must NOT be treated as half-dead. An old StartedAt with a live process IS
// half-dead; a legacy pidfile without startedAt is conservatively not.
func TestHalfDeadDaemonHoldsLockStartupGrace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	pidPath := PidPath(root)

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	base := LockInfo{
		PID:        cmd.Process.Pid,
		Version:    PackageVersion,
		SocketPath: PreferredSocket(root),
		StartedAt:  time.Now().Add(-time.Hour).UnixMilli(),
		ProcStart:  procStartTime(cmd.Process.Pid),
	}

	// Old StartedAt + live process → half-dead (killable).
	if err := os.WriteFile(pidPath, EncodeLock(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if !halfDeadDaemonHoldsLock(root) {
		t.Fatal("old StartedAt with a live process must be half-dead")
	}

	// Fresh StartedAt (startup window) + live process → NOT half-dead.
	base.StartedAt = time.Now().UnixMilli()
	if err := os.WriteFile(pidPath, EncodeLock(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if halfDeadDaemonHoldsLock(root) {
		t.Fatal("pidfile written within the startup grace must not be half-dead (healthy daemon still binding its socket)")
	}

	// Legacy pidfile without startedAt → conservative: NOT half-dead.
	base.StartedAt = 0
	if err := os.WriteFile(pidPath, EncodeLock(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if halfDeadDaemonHoldsLock(root) {
		t.Fatal("legacy pidfile without startedAt must not be half-dead (avoid killing a healthy daemon)")
	}

	// Dead process → not half-dead regardless of age.
	dead := LockInfo{PID: 1 << 30, Version: PackageVersion, StartedAt: time.Now().Add(-time.Hour).UnixMilli()}
	if err := os.WriteFile(pidPath, EncodeLock(dead), 0o600); err != nil {
		t.Fatal(err)
	}
	if halfDeadDaemonHoldsLock(root) {
		t.Fatal("dead process must not be half-dead")
	}

	_ = cmd.Process.Kill()
	<-waited
}

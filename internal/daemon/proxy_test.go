package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// TestEnsureAndDialSelfHealsInvisibleHolder (invisible-flock-holder
// regression): every candidate socket is unreachable, no pidfile exists, yet
// the DB flock is held by a live daemon-mode process whose pidfile/socket
// dentry were removed (deploy race). EnsureAndDial must: spawn (fails to
// come up on the held flock), detect via the db.Open probe that the flock is
// held, kill the invisible holder by /proc inspection, respawn once, and
// succeed.
func TestEnsureAndDialSelfHealsInvisibleHolder(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(CodeGraphDir(root), "codegraph.lock")

	// The invisible holder: a separate process that holds the DB flock. The
	// test binary re-executes itself as a tiny flock-holding helper so the
	// lock is owned by another process (killing it must release the flock).
	helper := exec.Command(os.Args[0], "-test.run=^TestHelperHoldFlock$")
	helper.Env = append(os.Environ(),
		"CG_HELPER_HOLD_FLOCK=1",
		"CG_HELPER_LOCK="+lockPath,
	)
	if err := helper.Start(); err != nil {
		t.Skipf("cannot start flock helper: %v", err)
	}
	helperDone := make(chan error, 1)
	go func() { helperDone <- helper.Wait() }()
	defer drainHelperKill(helper.Process, helperDone)

	// Wait until the helper actually holds the flock (flock -n equivalent:
	// a probe that fails to acquire means someone holds it).
	deadline := time.Now().Add(5 * time.Second)
	held := false
	for time.Now().Before(deadline) {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err == nil {
			if ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil {
				held = true
			}
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}
		if held {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !held {
		drainHelperKill(helper.Process, helperDone)
		t.Fatal("flock helper never acquired the lock")
	}

	// Fake /proc: the helper pid looks like a daemon-mode codegraph process
	// for this root (its real cmdline is the test binary — irrelevant, the
	// decision uses the injected snapshot). The pre-signal live recheck
	// (which normally reads the REAL /proc) is injected with the same
	// identity so terminateInvisibleHolder confirms and actually signals
	// the helper.
	withFakeProcs(t, []procSnapshot{matchingSnapshot(helper.Process.Pid, root)})
	withRecheckData(t, matchingSnapshot(helper.Process.Pid, root))

	// Fake spawner: the FIRST spawn produces no daemon (the poll fails, as
	// it would against a held flock); the SECOND spawn — after the invisible
	// holder is killed — brings up a fake daemon socket so the dial succeeds.
	origSpawn := spawnDetachedFn
	var ln net.Listener
	spawnCalls := 0
	spawnDetachedFn = func(r string, opts *SpawnOpts) error {
		spawnCalls++
		if spawnCalls == 1 {
			return nil
		}
		if err := os.MkdirAll(CodeGraphDir(r), 0o755); err != nil {
			return err
		}
		sock := PreferredSocket(r)
		l, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		ln = l
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					hello := fmt.Sprintf(`{"codegraph":%q,"pid":424242,"socketPath":%q,"protocol":1}`+"\n", PackageVersion, sock)
					_, _ = c.Write([]byte(hello))
					_, _ = io.Copy(io.Discard, c)
				}(c)
			}
		}()
		return nil
	}
	defer func() {
		spawnDetachedFn = origSpawn
		if ln != nil {
			_ = ln.Close()
			_ = os.Remove(PreferredSocket(root))
		}
	}()

	conn, br, hello, ok, err := EnsureAndDial(root, 300*time.Millisecond, 10*time.Millisecond, nil)
	if !ok || err != nil {
		t.Fatalf("EnsureAndDial self-heal: ok=%v err=%v", ok, err)
	}
	if conn == nil || br == nil {
		t.Fatal("nil conn/br on success")
	}
	if hello.PID != 424242 {
		t.Fatalf("hello pid = %d, want the fake daemon pid", hello.PID)
	}
	if spawnCalls != 2 {
		t.Fatalf("spawn calls = %d, want 2 (original + self-heal respawn)", spawnCalls)
	}
	// The invisible holder must have been terminated (flock released).
	select {
	case <-helperDone:
		t.Log("invisible holder terminated by self-heal")
	default:
		t.Fatal("invisible holder still running after self-heal")
	}
	_ = conn.Close()
}

// TestEnsureAndDialFlockFreeWalksOriginalPath: when the post-spawn probe
// shows the flock is FREE (the normal case — the spawn just failed for an
// unrelated reason), EnsureAndDial must NOT run the invisible-holder
// cleanup: no second spawn, no kills, plain fallback (ok=false, err=nil).
func TestEnsureAndDialFlockFreeWalksOriginalPath(t *testing.T) {
	root := t.TempDir()

	spawnCalls := 0
	origSpawn := spawnDetachedFn
	spawnDetachedFn = func(string, *SpawnOpts) error {
		spawnCalls++
		return nil
	}
	t.Cleanup(func() { spawnDetachedFn = origSpawn })

	conn, br, hello, ok, err := EnsureAndDial(root, 150*time.Millisecond, 10*time.Millisecond, nil)
	if ok {
		t.Fatal("EnsureAndDial succeeded although no daemon came up")
	}
	if err != nil {
		t.Fatalf("flock-free path must return nil error, got %v", err)
	}
	if spawnCalls != 1 {
		t.Fatalf("spawn calls = %d, want 1 (no self-heal respawn when flock is free)", spawnCalls)
	}
	if conn != nil || br != nil || hello.PID != 0 {
		t.Fatalf("unexpected outputs on fallback: conn=%v br=%v hello=%+v", conn, br, hello)
	}
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

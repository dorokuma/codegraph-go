package daemon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// TestKillStaleDaemonSignalsLiveProcess (upgrade-cleanup non-regression):
// KillStaleDaemon SIGTERMs the live process named in the pidfile — a REAL
// daemon of this project (CODEGRAPH_DAEMON_INTERNAL=1 plus -workdir root, the
// shape SpawnDetached has always produced) — waits for it to exit, then clears
// the lock.
func TestKillStaleDaemonSignalsLiveProcess(t *testing.T) {
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// The exe (/usr/bin/sh) does not match this test binary, so the environ
	// fact plus the workdir binding (and the pidfile start time) are what
	// authorize the kill — never the argv[0].
	cmd := startDaemonLikeProcess(t, root, true)
	// Reap the child in the background — like a real daemon (reparented via
	// SpawnDetached's Process.Release), the helper must not linger as a
	// zombie or IsProcessAlive would see it as still alive.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer cmd.Process.Kill() //nolint:errcheck
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
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

func TestKillStaleDaemonRefusesPIDReuse(t *testing.T) {
	// S3: the pidfile names a live process whose recorded start time no
	// longer matches /proc (the pid was recycled by an unrelated process).
	// KillStaleDaemon must NOT signal it and must NOT remove the lock;
	// it returns a clear error instead.
	if procStartTime(os.Getpid()) == 0 && procCmdline(os.Getpid()) == "" {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// ProcStart=1 can never match a real process started after boot: this is
	// the PID-reuse window (a different process now owns the pid).
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: 1}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal when recorded start time does not match")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	// The unrelated process must still be running and the lock still present.
	select {
	case <-done:
		t.Fatal("helper process was killed despite identity mismatch")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite identity mismatch: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

func TestIsCodegraphCmdline(t *testing.T) {
	cases := []struct {
		cmdline string
		want    bool
	}{
		{"codegraph\x00-workdir /x", true},
		{"codegraph-go\x00-workdir /x", true},
		{"/usr/local/bin/codegraph\x00-workdir /x", true},
		{"./codegraph-go\x00-workdir /x", true},
		{"/opt/my tools/codegraph-go\x00-workdir /x", true}, // argv[0] path with spaces
		{"sleep\x001000", false},                            // unrelated process
		{"/opt/editor-codegraph/editor\x00/x", false},       // substring only, not a segment
		{"codegraph-editor\x00", false},                     // basename is not exactly codegraph
		{"codegraph-server\x00", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isCodegraphCmdline(c.cmdline); got != c.want {
			t.Errorf("isCodegraphCmdline(%q) = %v, want %v", c.cmdline, got, c.want)
		}
	}
}

// TestKillStaleDaemonRefusesUnrelatedCmdline: a legacy pidfile without a
// recorded start time (ProcStart==0) never enters the kill path (fail closed:
// PID reuse cannot be ruled out) — the live process is refused regardless of
// its argv: no signal, no lock removal.
func TestKillStaleDaemonRefusesUnrelatedCmdline(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 && procCmdline(os.Getpid()) == "" {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal for unrelated process cmdline")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	// The unrelated process must still be running and the lock still present.
	select {
	case <-done:
		t.Fatal("unrelated process was killed despite identity mismatch")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite identity mismatch: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestKillStaleDaemonRefusesCodegraphSubstringCmdline: a legacy pidfile is
// refused even when its live process LOOKS like a codegraph binary (argv[0]
// merely contains "codegraph", e.g. an editor installed under
// .../codegraph-editor/) — without a recorded start time the kill path is
// closed entirely, so no cmdline heuristic can authorize a signal.
func TestKillStaleDaemonRefusesCodegraphSubstringCmdline(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 && procCmdline(os.Getpid()) == "" {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.Args[0] = "/opt/editor-codegraph/editor" // path contains the word, binary is not codegraph
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal for codegraph-substring cmdline")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	select {
	case <-done:
		t.Fatal("unrelated process was killed despite identity mismatch")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite identity mismatch: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestKillStaleDaemonRefusesUnreadableStartTime: the pidfile records a start
// time (ProcStart>0) but /proc no longer reports one — data is insufficient,
// so verification must refuse instead of degrading to the weaker cmdline
// check (never risk SIGTERMing a recycled pid).
func TestKillStaleDaemonRefusesUnreadableStartTime(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 && procCmdline(os.Getpid()) == "" {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate /proc becoming unreadable (permission change, procfs unmounted)
	// while the process is still alive.
	orig := procStartTimeFn
	procStartTimeFn = func(int) int64 { return 0 }
	defer func() { procStartTimeFn = orig }()

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal when start time is unreadable")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	select {
	case <-done:
		t.Fatal("process was killed despite unreadable start time")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite unreadable start time: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestKillStaleDaemonSIGKILLFallback (M3 regression): a live daemon whose
// identity is confirmed but which does not exit on SIGTERM must be escalated
// to SIGKILL after the grace window, the lock cleared, and a nil returned —
// never a fake success while the process is still alive. A SIGSTOPped helper
// models the stuck daemon: SIGTERM stays pending (never handled) while
// SIGKILL terminates it immediately.
func TestKillStaleDaemonSIGKILLFallback(t *testing.T) {
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Model a real spawned daemon: identity must pass via the
	// CODEGRAPH_DAEMON_INTERNAL=1 environ marker plus the -workdir binding
	// before the kill path runs.
	cmd := startDaemonLikeProcess(t, root, true)
	defer cmd.Process.Kill() //nolint:errcheck
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	// Stop the helper so it cannot handle SIGTERM (stays pending, process
	// stays alive) — the same shape as a daemon stuck in Stop that ignores
	// the graceful signal.
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP helper: %v", err)
	}
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := KillStaleDaemon(root); err != nil {
		t.Fatalf("KillStaleDaemon with SIGKILL fallback: %v", err)
	}
	// The helper must have been terminated (SIGKILL) and the lock cleared.
	select {
	case werr := <-waited:
		if werr == nil {
			t.Fatal("helper process still running after SIGKILL fallback")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper process not terminated after SIGKILL fallback")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("stale pidfile not cleared after SIGKILL fallback")
	}
}

// TestKillStaleDaemonRefusesPIDReuseAfterSIGTERMGrace (PID-reuse TOCTOU on
// the SIGKILL escalation): the daemon survives the SIGTERM grace window but
// its pid has been recycled by an unrelated process in the meantime. The
// pre-SIGKILL identity recheck must detect the change and REFUSE to escalate:
// no SIGKILL, no lock removal, and a clear ErrStaleDaemonRefused error — the
// unrelated process must never be hit. Deterministic: the helper is SIGSTOPped
// (SIGTERM stays pending, process stays alive), waitForExitFn is injected to
// report grace expiry instantly, and procStartTimeFn is injected to return the
// recorded start time for the first (pre-SIGTERM) verification and a different
// one for the recheck (simulating pid reuse during the grace window).
func TestKillStaleDaemonRefusesPIDReuseAfterSIGTERMGrace(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 && procCmdline(os.Getpid()) == "" {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Model a real spawned daemon: SpawnDetached marks it with
	// CODEGRAPH_DAEMON_INTERNAL=1 and its cmdline carries -workdir root, so
	// the first (pre-SIGTERM) verification passes on the real /proc data and
	// only the injected start-time change can trigger the recheck refusal.
	cmd := startDaemonLikeProcess(t, root, true)
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Stop the helper so the real SIGTERM stays pending and the process stays
	// alive — the same shape as a daemon stuck in Stop that ignores the
	// graceful signal, but here the injected recheck will "see" the pid
	// recycled.
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP helper: %v", err)
	}
	realStart := procStartTime(cmd.Process.Pid)
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: realStart}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	// SIGTERM grace expires immediately (injected), then the pre-SIGKILL
	// recheck reports a changed start time (pid reused by another process).
	origWait := waitForExitFn
	waitForExitFn = func(int, time.Duration) bool { return false }
	defer func() { waitForExitFn = origWait }()
	calls := 0
	origStart := procStartTimeFn
	procStartTimeFn = func(pid int) int64 {
		calls++
		if calls == 1 {
			return realStart // first verification (pre-SIGTERM) passes
		}
		return realStart + 1 // recheck after grace: identity changed
	}
	defer func() { procStartTimeFn = origStart }()

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal when the pid was recycled during the SIGTERM grace window")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	if calls < 2 {
		t.Fatalf("identity must be verified twice (pre-SIGTERM + pre-SIGKILL), got %d calls", calls)
	}
	// The unrelated process must still be running (no SIGKILL was sent) and
	// the lock still present.
	select {
	case <-done:
		t.Fatal("helper process was SIGKILLed despite identity mismatch after the grace window")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite identity mismatch: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestKillStaleDaemonClearsLockWhenDeadAfterGrace (PID-reuse TOCTOU, dead
// branch): the daemon survives the SIGTERM grace window (SIGTERM stays
// pending on a SIGSTOPped helper) but exits right after it. The pre-SIGKILL
// identity recheck then fails — /proc no longer reports the process (start
// time unreadable) — and the pid is DEAD, not recycled. KillStaleDaemon must
// clear the stale pidfile and return nil (the daemon did exit, just not
// within the grace window): no error, no SIGKILL escalation, no lock left
// behind. Deterministic: the injected waitForExitFn resumes the helper so the
// pending SIGTERM terminates it (death NOT caused by SIGKILL) and reports the
// grace expired; the real /proc readers then see a dead pid.
func TestKillStaleDaemonClearsLockWhenDeadAfterGrace(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 && procCmdline(os.Getpid()) == "" {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Model a real spawned daemon: SpawnDetached marks it with
	// CODEGRAPH_DAEMON_INTERNAL=1 and its cmdline carries -workdir root, so
	// the verifications pass on the real /proc data.
	cmd := startDaemonLikeProcess(t, root, true)
	defer cmd.Process.Kill() //nolint:errcheck
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	// Stop the helper so the real SIGTERM stays pending and the process stays
	// alive through the grace window — the same shape as a daemon stuck in
	// Stop that ignores the graceful signal.
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP helper: %v", err)
	}
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	// SIGTERM grace expires "immediately"; before returning, resume the
	// helper so the pending SIGTERM terminates it and wait for the exit to
	// complete — the recheck must observe a dead pid.
	origWait := waitForExitFn
	waitForExitFn = func(pid int, _ time.Duration) bool {
		if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
			t.Errorf("SIGCONT helper: %v", err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for IsProcessAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if IsProcessAlive(pid) {
			t.Error("helper still alive after SIGCONT+SIGTERM")
		}
		return false // grace window expired
	}
	defer func() { waitForExitFn = origWait }()

	if err := KillStaleDaemon(root); err != nil {
		t.Fatalf("KillStaleDaemon: want nil after daemon died post-grace, got %v", err)
	}
	// The helper must have died from the pending SIGTERM — NOT from a SIGKILL
	// escalation (the failed recheck with a dead pid must return before any
	// SIGKILL is sent).
	select {
	case werr := <-waited:
		if werr == nil {
			t.Fatal("helper process still running after KillStaleDaemon")
		}
		if ws, ok := werr.(*exec.ExitError).Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			t.Fatal("helper was SIGKILLed; the dead-pid branch must not escalate")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper process did not exit after KillStaleDaemon")
	}
	// The stale lock must be cleared (the daemon did exit, just late).
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("stale pidfile not cleared after daemon died post-grace")
	}
}

// startDaemonLikeProcess starts a live helper that models a SpawnDetached
// daemon for identity purposes: its /proc/<pid>/cmdline carries "-workdir
// root" as separate argv elements (the flag SpawnDetached has always passed —
// spawn.go), optionally with the CODEGRAPH_DAEMON_INTERNAL=1 environ marker.
// The shell wrapper backgrounded a sleeping child and traps SIGTERM, so a
// killed helper exits promptly like a real daemon (and takes its child along);
// the whole process group is SIGKILLed on cleanup so nothing outlives the
// test. The ambient CODEGRAPH_DAEMON_INTERNAL marker is stripped from the
// child environment before the optional marker is added.
func startDaemonLikeProcess(t *testing.T, root string, daemonMarker bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 30 & p=$!; trap 'kill $p 2>/dev/null' TERM; wait $p", "sh", "-workdir", root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, EnvDaemonInternal+"=") {
			continue
		}
		env = append(env, e)
	}
	if daemonMarker {
		env = append(env, EnvDaemonInternal+"=1")
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	// /proc/<pid>/stat starttime is fixed at fork and survives exec, but
	// /proc/<pid>/cmdline shows the pre-exec (test binary's) argv until
	// execve lands. A reader inside that fork->exec window sees no
	// "-workdir" and verifyDaemonIdentity refuses with "not a daemon of
	// this project" even though the helper is healthy — observed as a CI
	// flake on slow 2-core runners (fast machines mask the window). Wait
	// for the target argv to become visible before handing the Cmd out.
	if procStartTime(os.Getpid()) != 0 {
		deadline := time.Now().Add(2 * time.Second)
		for {
			raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid))
			if err == nil && cmdlineWorkdirMatches(string(raw), root) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("helper cmdline never showed -workdir %s (last %q, err %v)", root, string(raw), err)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
	return cmd
}

// TestCmdlineWorkdirMatches: the -workdir parse and Clean normalization used
// by the stale-kill identity check (same shape as isInvisibleHolder in
// recovery.go: first argv element exactly "-workdir", next element is the
// value, filepath.Clean equality).
func TestCmdlineWorkdirMatches(t *testing.T) {
	cases := []struct {
		cmdline string
		root    string
		want    bool
	}{
		{"codegraph-go\x00-workdir\x00/tmp/x", "/tmp/x", true},
		{"codegraph-go\x00-workdir\x00/tmp/x/", "/tmp/x", true}, // Clean tolerates a trailing slash
		{"codegraph-go\x00-workdir\x00/tmp/x", "/tmp/x/", true}, // ... on either side
		{"sh\x00-c\x00sleep 30\x00sh\x00-workdir\x00/tmp/x", "/tmp/x", true},
		{"codegraph-go\x00-workdir\x00/tmp/xy", "/tmp/x", false},                      // path prefix is not equality
		{"codegraph-go\x00-workdir\x00/tmp/x\x00-workdir\x00/other", "/tmp/x", true},  // first occurrence wins
		{"codegraph-go\x00-workdir\x00/other\x00-workdir\x00/tmp/x", "/tmp/x", false}, // first occurrence wins
		{"codegraph-go\x00-workdir", "/tmp/x", false},                                 // flag without a value
		{"codegraph-go\x00", "/tmp/x", false},                                         // no flag at all
		{"", "/tmp/x", false},                                                         // empty cmdline (zombie)
		{"codegraph-go\x00-workdir\x00", "/tmp/x", false},                             // empty value
		{"codegraph-go\x00-workdir\x00/tmp/x", "", false},                             // empty root
	}
	for _, c := range cases {
		if got := cmdlineWorkdirMatches(c.cmdline, c.root); got != c.want {
			t.Errorf("cmdlineWorkdirMatches(%q, %q) = %v, want %v", c.cmdline, c.root, got, c.want)
		}
	}
}

// TestVerifyDaemonIdentitySameProjectDaemon (predicate-level, full real /proc
// profile): a helper whose cmdline carries -workdir rootA and whose environ
// carries CODEGRAPH_DAEMON_INTERNAL=1 — the exact shape SpawnDetached produces
// — passes verification for rootA and fails for another root with the
// workdir-binding refusal (cross-project binding).
func TestVerifyDaemonIdentitySameProjectDaemon(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	rootA := t.TempDir()
	rootB := t.TempDir()
	cmd := startDaemonLikeProcess(t, rootA, true)
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(rootA), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := verifyDaemonIdentity(&info, rootA); err != nil {
		t.Fatalf("same-project daemon must pass verification: %v", err)
	}
	err := verifyDaemonIdentity(&info, rootB)
	if err == nil {
		t.Fatal("same daemon must fail verification for another project root")
	}
	if !strings.Contains(err.Error(), "not a daemon of this project") {
		t.Fatalf("expected the workdir-binding refusal, got: %v", err)
	}
}

// TestKillStaleDaemonRefusesCrossProjectPidfile (second-round audit: the
// cross-project harvest): project A runs a REAL daemon (marker + -workdir
// rootA). An attacker inside project B forges B's pidfile with the daemon's
// real pid and /proc start time — everything the pre-workdir check needed.
// The -workdir binding must refuse the kill: A's daemon survives and B's
// pidfile stays in place.
func TestKillStaleDaemonRefusesCrossProjectPidfile(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	rootA := t.TempDir()
	rootB := t.TempDir()
	pidPathB := PidPath(rootB)
	if err := os.MkdirAll(filepath.Dir(pidPathB), 0o755); err != nil {
		t.Fatal(err)
	}
	// Project A's real daemon (marker + -workdir rootA).
	cmd := startDaemonLikeProcess(t, rootA, true)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Forged inside project B: real pid, real /proc start time, stale version.
	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(rootB), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPathB, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(rootB)
	if err == nil {
		t.Fatal("expected refusal when project B's pidfile names project A's daemon")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	select {
	case <-done:
		t.Fatal("project A's daemon was killed via project B's forged pidfile")
	default:
	}
	if _, serr := os.Stat(pidPathB); serr != nil {
		t.Fatalf("lock removed despite cross-project mismatch: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestKillStaleDaemonRefusesEnvironMarkerWithoutWorkdir (second-round audit:
// the environ single fact no longer authorizes a kill): a live process
// carrying CODEGRAPH_DAEMON_INTERNAL=1 (e.g. an innocent `env VAR=1 sleep`)
// but whose cmdline has no -workdir must be refused even with a fully
// forged-but-consistent pidfile (real pid, real /proc start time).
func TestKillStaleDaemonRefusesEnvironMarkerWithoutWorkdir(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Innocent victim with the daemon marker but no daemon cmdline shape.
	cmd := exec.Command("sleep", "30")
	cmd.Env = append(os.Environ(), EnvDaemonInternal+"=1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal for a marker-carrying process without -workdir")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	select {
	case <-done:
		t.Fatal("marked process was killed without the workdir binding")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite missing workdir binding: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestKillStaleDaemonRefusesSameBinaryWithoutWorkdir (second-round audit: the
// exe single fact no longer authorizes a kill): a foreground process of the
// SAME binary — a direct-mode client — whose cmdline carries no -workdir for
// this project must be refused even with a forged-but-consistent pidfile. The
// exe fact is stubbed in (osExecutableFn -> the helper's real binary path),
// so without the workdir binding this would previously have been killed.
func TestKillStaleDaemonRefusesSameBinaryWithoutWorkdir(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer cmd.Process.Kill() //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// /proc/<pid>/exe readlink yields the fully resolved binary path.
	exe, err := filepath.EvalSymlinks(cmd.Path)
	if err != nil {
		t.Fatalf("resolve helper exe: %v", err)
	}
	origExec := osExecutableFn
	osExecutableFn = func() (string, error) { return exe, nil }
	defer func() { osExecutableFn = origExec }()

	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err = KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal for a same-binary process without -workdir")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	select {
	case <-done:
		t.Fatal("same-binary process was killed without the workdir binding")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite missing workdir binding: %v", serr)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestKillStaleDaemonSignalsLiveProcessViaKillFallback: with the pidfd path
// forced unavailable (pidfd_open -> ErrPidfdNotSupported — the non-Linux /
// old-kernel condition), the classic kill(2)+recheck fallback still
// terminates a same-project daemon (marker + -workdir) and clears the lock.
func TestKillStaleDaemonSignalsLiveProcessViaKillFallback(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := startDaemonLikeProcess(t, root, true)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer cmd.Process.Kill() //nolint:errcheck

	origOpen := pidfdOpenFn
	pidfdOpenFn = func(int) (int, error) { return -1, ErrPidfdNotSupported }
	defer func() { pidfdOpenFn = origOpen }()

	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := KillStaleDaemon(root); err != nil {
		t.Fatalf("KillStaleDaemon via kill(2) fallback: %v", err)
	}
	select {
	case werr := <-waited:
		if werr == nil {
			t.Fatal("helper process still running after KillStaleDaemon fallback")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper process not terminated after KillStaleDaemon fallback")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("stale pidfile not cleared after fallback kill")
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

// startInnocentProcess starts a plain helper process with NO daemon identity:
// the ambient CODEGRAPH_DAEMON_INTERNAL marker (if any) is stripped from its
// environment. argv0 optionally spoofs the process name, modeling attacks
// like `exec -a codegraph-go ...`. The caller owns the process and must
// kill (and reap) it.
func startInnocentProcess(t *testing.T, argv0 string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if argv0 != "" {
		cmd.Args[0] = argv0
	}
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, EnvDaemonInternal+"=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	return cmd
}

// waitZombieState polls until /proc/<pid>/stat reports state Z (exited but
// not yet reaped), failing the test after a short deadline.
func waitZombieState(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil {
			if i := bytes.LastIndexByte(raw, ')'); i >= 0 {
				fields := strings.Fields(string(raw[i+2:]))
				if len(fields) > 0 && fields[0] == "Z" {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper process never became a zombie")
}

// TestKillStaleDaemonRefusesForgedPidfileForInnocentProcess (audit: pidfile
// forgery): any same-user process can write a daemon.pid naming an arbitrary
// victim pid together with the victim's real /proc start time (pid and
// starttime are world-readable in /proc) and delete the socket to manufacture
// a stale-daemon scenario. The start-time match alone must NOT authorize the
// kill: the target is a plain sleep with no daemon identity (its exe is
// /usr/bin/sleep, its environ carries no CODEGRAPH_DAEMON_INTERNAL), so
// KillStaleDaemon must refuse, leave the victim running, and keep the pidfile.
func TestKillStaleDaemonRefusesForgedPidfileForInnocentProcess(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := startInnocentProcess(t, "")
	defer func() { _ = victim.Process.Kill() }()
	done := make(chan error, 1)
	go func() { done <- victim.Wait() }()

	// Fully forged-but-consistent pidfile: real pid, real /proc start time.
	info := LockInfo{PID: victim.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(victim.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal for a pidfile forged to name an innocent process")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	select {
	case <-done:
		t.Fatal("innocent process was killed via a forged pidfile")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite forged identity: %v", serr)
	}
	_ = victim.Process.Kill()
	<-done
}

// TestKillStaleDaemonRefusesLegacyPidfileWithoutProcStart (audit: fail
// closed): a legacy pidfile without a recorded start time cannot be checked
// for PID reuse, so it must never enter the kill path — not even through the
// old cmdline inspection, which an attacker could satisfy with
// `exec -a codegraph-go ...` and which passed silently for empty cmdlines
// (zombies). The live process must be refused and left running; the pidfile
// is cleared only by the normal ClearStaleLock path once the process is dead.
func TestKillStaleDaemonRefusesLegacyPidfileWithoutProcStart(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// argv[0] spoofed to a codegraph name — exactly the shape that passed the
	// old cmdline identity check.
	victim := startInnocentProcess(t, "codegraph-go")
	defer func() { _ = victim.Process.Kill() }()
	done := make(chan error, 1)
	go func() { done <- victim.Wait() }()

	info := LockInfo{PID: victim.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1} // no ProcStart
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal for a legacy pidfile without a recorded start time")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	select {
	case <-done:
		t.Fatal("process was killed via a legacy pidfile")
	default:
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite legacy pidfile: %v", serr)
	}
	_ = victim.Process.Kill()
	<-done
}

// TestVerifyDaemonIdentityAcceptsSelf: a pidfile naming THIS live process
// with the correct /proc start time passes verification — /proc/<pid>/exe
// resolves to the running binary, which is the first identity fact (a real
// daemon is spawned from os.Executable() by SpawnDetached, so client and
// daemon share one binary). The test binary's own argv carries no -workdir,
// so the cmdline reader is stubbed to the shape SpawnDetached produces; the
// full-profile pass with real /proc data is covered by
// TestVerifyDaemonIdentitySameProjectDaemon. Real /proc for start time and
// exe, no signal sent — this exercises the predicate in isolation.
func TestVerifyDaemonIdentityAcceptsSelf(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	origCmdline := procCmdlineFn
	procCmdlineFn = func(int) string { return "codegraph-go\x00-workdir\x00" + root }
	defer func() { procCmdlineFn = origCmdline }()
	info := LockInfo{PID: os.Getpid(), Version: PackageVersion, SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(os.Getpid())}
	if err := verifyDaemonIdentity(&info, root); err != nil {
		t.Fatalf("self must pass identity verification: %v", err)
	}
}

// TestKillStaleDaemonKillsSameBinaryDaemon (identity fact #1 — the target's
// /proc/<pid>/exe resolves to this binary): the helper is a same-project
// daemon-shaped process (-workdir root in its cmdline, no environ marker) and
// osExecutableFn is stubbed to the helper's real exe path to model a
// client/daemon pair sharing one binary. Full flow: SIGTERM -> exit -> lock
// cleared. A same-binary process WITHOUT the -workdir binding is refused —
// see TestKillStaleDaemonRefusesSameBinaryWithoutWorkdir.
func TestKillStaleDaemonKillsSameBinaryDaemon(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := startDaemonLikeProcess(t, root, false)
	defer cmd.Process.Kill() //nolint:errcheck
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	// /proc/<pid>/exe readlink yields the fully resolved binary path.
	exe, err := filepath.EvalSymlinks(cmd.Path)
	if err != nil {
		t.Fatalf("resolve helper exe: %v", err)
	}
	origExec := osExecutableFn
	osExecutableFn = func() (string, error) { return exe, nil }
	defer func() { osExecutableFn = origExec }()

	info := LockInfo{PID: cmd.Process.Pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: procStartTime(cmd.Process.Pid)}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := KillStaleDaemon(root); err != nil {
		t.Fatalf("KillStaleDaemon with matching exe: %v", err)
	}
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

// TestKillStaleDaemonRefusesZombieTarget: a zombie keeps its pid and its
// /proc start time (stat is still readable), but neither its exe nor its
// environ is readable — identity is unverifiable, so the kill must be
// refused. (The old code passed empty-cmdline processes straight through for
// legacy pidfiles, and a matching start time alone would have authorized the
// kill for modern ones.)
func TestKillStaleDaemonRefusesZombieTarget(t *testing.T) {
	if procStartTime(os.Getpid()) == 0 {
		t.Skip("no /proc on this platform; identity verification unavailable")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	start := procStartTime(pid)
	// Kill the child WITHOUT reaping it: it stays as a zombie (state Z) with
	// its /proc entry intact.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	waitZombieState(t, pid)
	if !IsProcessAlive(pid) {
		t.Fatal("zombie must count as alive for kill(2)")
	}

	info := LockInfo{PID: pid, Version: "0.0.0", SocketPath: PreferredSocket(root), StartedAt: 1, ProcStart: start}
	if err := os.WriteFile(pidPath, EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}

	err := KillStaleDaemon(root)
	if err == nil {
		t.Fatal("expected refusal for a zombie target (identity unverifiable)")
	}
	if !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("expected a clear refusal message, got: %v", err)
	}
	if _, serr := os.Stat(pidPath); serr != nil {
		t.Fatalf("lock removed despite unverifiable identity: %v", serr)
	}
	// Reap the zombie.
	_ = cmd.Wait()
}

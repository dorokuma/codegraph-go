package daemon

import (
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

func TestKillStaleDaemonSignalsLiveProcess(t *testing.T) {
	// B1: KillStaleDaemon SIGTERMs the live process named in the pidfile,
	// waits for it to exit, then clears the lock.
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.Args[0] = "codegraph-go" // pass the cmdline identity check on procfs platforms
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
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

// TestKillStaleDaemonRefusesUnrelatedCmdline: an older pidfile without a
// recorded start time (ProcStart==0) whose live process is unrelated — its
// argv[0] is not a codegraph binary — must be refused: no signal, no lock
// removal.
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

// TestKillStaleDaemonRefusesCodegraphSubstringCmdline: an unrelated process
// whose argv[0] merely CONTAINS "codegraph" (e.g. an editor installed under
// .../codegraph-editor/) must not pass the identity check — only exact
// basename/path-segment matches count.
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
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
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
	cmd := exec.Command("sleep", "30")
	cmd.Args[0] = "codegraph-go" // pass the cmdline identity check on procfs platforms
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
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
	cmd := exec.Command("sleep", "30")
	cmd.Args[0] = "codegraph-go" // pass the cmdline identity check on procfs platforms
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
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

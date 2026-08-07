package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// startSleepHelper spawns a long-lived `sleep` whose argv[0] is spoofed to
// "codegraph-go" so it passes cmdline-based identity checks on procfs
// platforms. The real /proc cmdline only matters for tests that do NOT
// inject a fake scan; the invisible-holder tests below always inject fake
// /proc data, so the spoof is for consistency with the other daemon tests.
func startSleepHelper(t *testing.T) (*exec.Cmd, chan error) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.Args[0] = "codegraph-go"
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done
}

// drainHelperKill kills the helper (if still alive) and drains its Wait
// channel exactly once. Safe to call whether or not the test already
// consumed done.
func drainHelperKill(proc *os.Process, done chan error) {
	_ = proc.Kill()
	select {
	case <-done:
	default:
	}
}

// withFakeProcs injects a synthetic /proc snapshot into killInvisibleHolders
// for the duration of the test.
func withFakeProcs(t *testing.T, procs []procSnapshot) {
	t.Helper()
	orig := scanProcsFn
	scanProcsFn = func() ([]procSnapshot, error) { return procs, nil }
	t.Cleanup(func() { scanProcsFn = orig })
}

// withRecheckData injects the live /proc cmdline/environ readers for ONE
// pid, so the pre-signal re-verification (terminateInvisibleHolder →
// liveInvisibleHolderMatch) sees the given data instead of the real /proc.
// All other pids keep the real readers. This is what lets tests exercise
// the snapshot-decision vs pre-signal-recheck distinction: the scan
// snapshot can say "matches" while the live recheck says "not anymore".
func withRecheckData(t *testing.T, p procSnapshot) {
	t.Helper()
	origCmdline := procCmdlineFn
	origEnviron := procEnvironFn
	procCmdlineFn = func(pid int) string {
		if pid == p.pid {
			return p.cmdline
		}
		return origCmdline(pid)
	}
	procEnvironFn = func(pid int) string {
		if pid == p.pid {
			return p.environ
		}
		return origEnviron(pid)
	}
	t.Cleanup(func() {
		procCmdlineFn = origCmdline
		procEnvironFn = origEnviron
	})
}

// assertHelperDead fails the test unless the helper process has exited.
func assertHelperDead(t *testing.T, done chan error, cmd *exec.Cmd) {
	t.Helper()
	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		drainHelperKill(cmd.Process, done)
		t.Fatal("helper process was not terminated")
	}
}

func matchingSnapshot(pid int, root string) procSnapshot {
	return procSnapshot{
		pid:     pid,
		cmdline: "codegraph-go\x00-workdir\x00" + root,
		environ: "PATH=/usr/bin\x00CODEGRAPH_DAEMON_INTERNAL=1\x00",
	}
}

// writeLockInfo writes a pidfile for root with the given body (used by the
// newborn-grace tests).
func writeLockInfo(t *testing.T, root string, info LockInfo) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(PidPath(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PidPath(root), EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPidfdPathSignalsChildOnLinux: the pidfd path is usable on a real
// Linux kernel — open a pidfd for a live child, SIGTERM it THROUGH the
// pidfd (pidfd_send_signal), and assert the child exits. Skipped when the
// kernel predates pidfd_open (ENOSYS).
func TestPidfdPathSignalsChildOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pidfd is Linux-only")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer drainHelperKill(cmd.Process, done)

	fd, err := pidfdOpenFn(cmd.Process.Pid)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			t.Skipf("kernel without pidfd_open: %v", err)
		}
		t.Fatalf("pidfdOpen: %v", err)
	}
	defer pidfdCloseFn(fd) //nolint:errcheck

	if err := pidfdSendSignalFn(fd, syscall.SIGTERM); err != nil {
		t.Fatalf("pidfdSendSignal(SIGTERM): %v", err)
	}
	select {
	case <-done:
		// exited via the pidfd signal
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit after pidfd SIGTERM")
	}
}

// TestKillInvisibleHoldersFallsBackWhenPidfdUnsupported: when pidfd_open is
// unavailable (non-Linux, ENOSYS, EPERM, ...), terminateInvisibleHolder must
// gracefully fall back to the classic kill(2)+Signal+recheck path
// and still terminate the holder. This pins the fallback coverage the audit
// asked for: the pidfd path is an optimization, never a requirement.
func TestKillInvisibleHoldersFallsBackWhenPidfdUnsupported(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)

	origOpen := pidfdOpenFn
	pidfdOpenFn = func(int) (int, error) { return -1, ErrPidfdNotSupported }
	t.Cleanup(func() { pidfdOpenFn = origOpen })

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders via fallback: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1 (kill(2) fallback must terminate the holder)", n)
	}
	assertHelperDead(t, done, cmd)
}

// TestKillInvisibleHoldersNewbornGrace (m3): root's pidfile names a LIVE
// process started less than halfDeadStartupGrace ago — a daemon spawned
// moments ago that is still binding its socket. The invisible-holder
// cleanup must SKIP the kill so the newborn gets time to finish binding
// (then the caller's dial loop connects) or to exit on its own (flock
// failure).
func TestKillInvisibleHoldersNewbornGrace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)
	// Fresh pidfile naming the LIVE helper, StartedAt = now → in grace.
	writeLockInfo(t, root, LockInfo{PID: cmd.Process.Pid, Version: PackageVersion, StartedAt: time.Now().UnixMilli()})

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 0 {
		t.Fatalf("killed = %d, want 0 (newborn daemon in startup grace must be skipped)", n)
	}
	select {
	case <-done:
		t.Fatal("newborn daemon was killed during startup grace")
	default:
	}
}

// TestKillInvisibleHoldersOldPidfileKills (m3): a pidfile whose StartedAt is
// older than halfDeadStartupGrace does NOT protect the holder — the cleanup
// proceeds and terminates it.
func TestKillInvisibleHoldersOldPidfileKills(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)
	writeLockInfo(t, root, LockInfo{PID: cmd.Process.Pid, Version: PackageVersion, StartedAt: time.Now().Add(-10 * time.Second).UnixMilli()})

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1 (old pidfile must not protect the holder)", n)
	}
	assertHelperDead(t, done, cmd)
}

// TestKillInvisibleHoldersLegacyPidfileKills (m3): a legacy pidfile without
// startedAt cannot be age-gated — no grace (same conservative choice as
// halfDeadDaemonHoldsLock), the holder is terminated.
func TestKillInvisibleHoldersLegacyPidfileKills(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)
	writeLockInfo(t, root, LockInfo{PID: cmd.Process.Pid, Version: PackageVersion}) // StartedAt = 0

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1 (legacy pidfile without startedAt must not protect the holder)", n)
	}
	assertHelperDead(t, done, cmd)
}

// TestKillInvisibleHoldersNoPidfileKills (m3): the classic wedge — no
// pidfile at all (deploy race removed it) — must be unaffected by the
// grace: the cleanup proceeds and terminates the holder.
func TestKillInvisibleHoldersNoPidfileKills(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1 (no pidfile → wedge scenario, no grace)", n)
	}
	assertHelperDead(t, done, cmd)
}

// TestKillInvisibleHoldersSurvivorWrapsSentinel (m5): a holder that is
// still alive after SIGTERM+SIGKILL (simulated: pidfd signals are injected
// as no-ops and waitForExitFn reports still-alive) must produce an error
// wrapping ErrInvisibleHolderSurvived, and the holder must NOT be counted
// as killed.
func TestKillInvisibleHoldersSurvivorWrapsSentinel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)

	origOpen, origSend, origWait := pidfdOpenFn, pidfdSendSignalFn, waitForExitFn
	pidfdOpenFn = func(int) (int, error) { return 7777, nil }
	pidfdSendSignalFn = func(fd int, sig syscall.Signal) error { return nil } // never actually signals
	waitForExitFn = func(pid int, d time.Duration) bool { return false }      // never exits
	t.Cleanup(func() { pidfdOpenFn, pidfdSendSignalFn, waitForExitFn = origOpen, origSend, origWait })

	n, err := killInvisibleHolders(root)
	if n != 0 {
		t.Fatalf("killed = %d, want 0 (survivor must not count as killed)", n)
	}
	if err == nil || !errors.Is(err, ErrInvisibleHolderSurvived) {
		t.Fatalf("want error wrapping ErrInvisibleHolderSurvived, got %v", err)
	}
	select {
	case <-done:
		t.Fatal("helper died although signals were injected as no-ops")
	default:
	}
}

// TestEnsureAndDialPropagatesSurvivorSentinel (m5): when the invisible-
// holder cleanup reports a survivor (SIGTERM+SIGKILL ineffective),
// EnsureAndDial must propagate the error as-is (wrapping
// ErrInvisibleHolderSurvived) instead of respawning or falling back — main
// turns it into an actionable exit.
func TestEnsureAndDialPropagatesSurvivorSentinel(t *testing.T) {
	root := t.TempDir()

	// Hold the flock so EnsureAndDial enters the invisible-holder branch.
	lockPath := filepath.Join(CodeGraphDir(root), "codegraph.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	// Fake /proc: a matching invisible holder (pid 424242 does not exist;
	// the injected recheck makes it look alive and matching).
	withFakeProcs(t, []procSnapshot{matchingSnapshot(424242, root)})
	withRecheckData(t, matchingSnapshot(424242, root))

	// Survivor simulation: pidfd opens, signals are no-ops, never exits.
	origOpen, origSend, origWait := pidfdOpenFn, pidfdSendSignalFn, waitForExitFn
	pidfdOpenFn = func(int) (int, error) { return 7777, nil }
	pidfdSendSignalFn = func(fd int, sig syscall.Signal) error { return nil }
	waitForExitFn = func(pid int, d time.Duration) bool { return false }
	t.Cleanup(func() { pidfdOpenFn, pidfdSendSignalFn, waitForExitFn = origOpen, origSend, origWait })

	origSpawn := spawnDetachedFn
	spawnDetachedFn = func(string, *SpawnOpts) error { return nil }
	t.Cleanup(func() { spawnDetachedFn = origSpawn })

	conn, br, hello, ok, err := EnsureAndDial(root, 200*time.Millisecond, 10*time.Millisecond, nil)
	if ok {
		t.Fatal("EnsureAndDial succeeded although the flock stayed held by a survivor")
	}
	if err == nil || !errors.Is(err, ErrInvisibleHolderSurvived) {
		t.Fatalf("EnsureAndDial must propagate ErrInvisibleHolderSurvived, got ok=%v err=%v", ok, err)
	}
	if conn != nil || br != nil || hello.PID != 0 {
		t.Fatalf("unexpected outputs on survivor error: conn=%v br=%v hello=%+v", conn, br, hello)
	}
}

// TestKillInvisibleHoldersKillsMatchingDaemon: a daemon-mode process
// (codegraph argv0 + -workdir root + CODEGRAPH_DAEMON_INTERNAL=1) that is
// unreachable through sockets/pidfiles must be terminated. The pre-signal
// live recheck is injected to confirm the same identity (the helper's real
// /proc argv is a `sleep`, not a codegraph daemon).
func TestKillInvisibleHoldersKillsMatchingDaemon(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1", n)
	}
	assertHelperDead(t, done, cmd)
}

// TestKillInvisibleHoldersKillsMultipleDaemons: every matching process is
// terminated, not just the first.
func TestKillInvisibleHoldersKillsMultipleDaemons(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd1, done1 := startSleepHelper(t)
	defer drainHelperKill(cmd1.Process, done1)
	cmd2, done2 := startSleepHelper(t)
	defer drainHelperKill(cmd2.Process, done2)

	snap1 := matchingSnapshot(cmd1.Process.Pid, root)
	snap2 := matchingSnapshot(cmd2.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap1, snap2})
	withRecheckData(t, snap1)
	withRecheckData(t, snap2)

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 2 {
		t.Fatalf("killed = %d, want 2", n)
	}
	assertHelperDead(t, done1, cmd1)
	assertHelperDead(t, done2, cmd2)
}

// TestKillInvisibleHoldersSkipsForegroundClient: a process whose argv
// matches but whose environ lacks CODEGRAPH_DAEMON_INTERNAL=1 is a
// foreground client or direct-mode session — it must never be touched.
func TestKillInvisibleHoldersSkipsForegroundClient(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	snap.environ = "PATH=/usr/bin\x00" // no CODEGRAPH_DAEMON_INTERNAL
	withFakeProcs(t, []procSnapshot{snap})

	if _, err := killInvisibleHolders(root); err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	select {
	case <-done:
		t.Fatal("foreground client was killed")
	default:
	}
}

// TestKillInvisibleHoldersSkipsSelf: our own pid must never be signaled,
// even when the rest of the profile matches.
func TestKillInvisibleHoldersSkipsSelf(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	withFakeProcs(t, []procSnapshot{matchingSnapshot(os.Getpid(), root)})

	if _, err := killInvisibleHolders(root); err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if !IsProcessAlive(os.Getpid()) {
		t.Fatal("killInvisibleHolders killed the calling process")
	}
}

// TestKillInvisibleHoldersSkipsWorkdirMismatch: a daemon-mode codegraph
// process for a DIFFERENT workdir holds a different lock and is healthy —
// it must not be killed.
func TestKillInvisibleHoldersSkipsWorkdirMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	snap.cmdline = "codegraph-go\x00-workdir\x00/other/root"
	withFakeProcs(t, []procSnapshot{snap})

	if _, err := killInvisibleHolders(root); err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	select {
	case <-done:
		t.Fatal("daemon of another workdir was killed")
	default:
	}
}

// TestKillInvisibleHoldersSkipsUnreadableEnviron: unreadable /proc environ
// must be treated conservatively — no match, no kill.
func TestKillInvisibleHoldersSkipsUnreadableEnviron(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	snap.environ = "" // unreadable
	withFakeProcs(t, []procSnapshot{snap})

	if _, err := killInvisibleHolders(root); err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	select {
	case <-done:
		t.Fatal("process with unreadable environ was killed")
	default:
	}
}

// TestKillInvisibleHoldersSkipsNonCodegraphCmdline: argv[0] that is not a
// codegraph binary must never match, even with a matching -workdir and
// daemon environ.
func TestKillInvisibleHoldersSkipsNonCodegraphCmdline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	snap.cmdline = "sleep\x0030"
	withFakeProcs(t, []procSnapshot{snap})

	if _, err := killInvisibleHolders(root); err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	select {
	case <-done:
		t.Fatal("non-codegraph process was killed")
	default:
	}
}

// TestKillInvisibleHoldersSIGKILLFallback: a holder that does not exit on
// SIGTERM (SIGSTOPped, like a daemon stuck in Stop) must be escalated to
// SIGKILL and reported as success — the flock must not stay held.
func TestKillInvisibleHoldersSIGKILLFallback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP helper: %v", err)
	}
	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders with SIGKILL fallback: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1", n)
	}
	assertHelperDead(t, done, cmd)
}

// TestKillInvisibleHoldersRecheckMismatchSkipsKill (PID-reuse TOCTOU): the
// scan snapshot matches the holder profile, but the pre-signal live recheck
// (injected) does NOT — the pid died and was recycled by an unrelated
// process between scan and signal. The process must not be signaled and
// must not be counted as killed: the result is 0.
func TestKillInvisibleHoldersRecheckMismatchSkipsKill(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	// The live recheck now sees this pid as an unrelated process.
	withRecheckData(t, procSnapshot{
		pid:     cmd.Process.Pid,
		cmdline: "some-other-tool\x00--flag",
		environ: "PATH=/usr/bin",
	})

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 0 {
		t.Fatalf("killed = %d, want 0 (recheck mismatch must not count)", n)
	}
	select {
	case <-done:
		t.Fatal("process was signaled although the live recheck did not match")
	default:
	}
}

// TestKillInvisibleHoldersRecheckMismatchContinues: a skipped candidate
// (recheck mismatch) must not abort the loop — the remaining candidates are
// still processed and the mismatched process is never signaled.
func TestKillInvisibleHoldersRecheckMismatchContinues(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")

	// Holder A: snapshot matches, live recheck disagrees (recycled pid).
	cmdA, doneA := startSleepHelper(t)
	defer drainHelperKill(cmdA.Process, doneA)
	// Holder B: snapshot AND live recheck match — must still be killed.
	cmdB, doneB := startSleepHelper(t)
	defer drainHelperKill(cmdB.Process, doneB)

	withFakeProcs(t, []procSnapshot{
		matchingSnapshot(cmdA.Process.Pid, root),
		matchingSnapshot(cmdB.Process.Pid, root),
	})
	withRecheckData(t, procSnapshot{
		pid:     cmdA.Process.Pid,
		cmdline: "some-other-tool\x00--flag",
		environ: "PATH=/usr/bin",
	})
	withRecheckData(t, matchingSnapshot(cmdB.Process.Pid, root))

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1 (only the recheck-confirmed holder)", n)
	}
	select {
	case <-doneA:
		t.Fatal("holder A was signaled although the live recheck did not match")
	default:
	}
	assertHelperDead(t, doneB, cmdB)
}

// TestKillInvisibleHoldersRecheckMatchKills: the pre-signal live recheck
// (injected) confirms the pid still matches the holder profile → the
// process is signaled and counted.
func TestKillInvisibleHoldersRecheckMatchKills(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	cmd, done := startSleepHelper(t)
	defer drainHelperKill(cmd.Process, done)

	snap := matchingSnapshot(cmd.Process.Pid, root)
	withFakeProcs(t, []procSnapshot{snap})
	withRecheckData(t, snap)

	n, err := killInvisibleHolders(root)
	if err != nil {
		t.Fatalf("killInvisibleHolders: %v", err)
	}
	if n != 1 {
		t.Fatalf("killed = %d, want 1", n)
	}
	assertHelperDead(t, done, cmd)
}

// TestInvisibleHolderHoldsLock: the lightweight flock probe reports true
// when another process holds the DB flock, and false when it is free or
// when the lock file cannot be opened (directory missing). The probe never
// CREATES the lock file (no O_CREATE): a free probe on a pristine project
// must leave no file behind.
func TestInvisibleHolderHoldsLock(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(CodeGraphDir(root), "codegraph.lock")

	// Free: no lock file exists yet — the probe must report free and must
	// NOT create the file (read-only probe, no side effects).
	if invisibleHolderHoldsLock(root) {
		t.Fatal("free flock reported as held")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("probe left a lock file behind (stat err=%v); it must not create files", err)
	}

	// Held: the test process itself takes the flock (killInvisibleHolders is
	// not involved here — only the probe is under test). The probe does NOT
	// create the .codegraph dir (missing dir = free), so the test creates it.
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	if !invisibleHolderHoldsLock(root) {
		t.Fatal("held flock reported as free")
	}

	// Directory missing: no lock file can exist, so nothing can be held —
	// the probe must report free (never "held" on an unopenable path).
	if invisibleHolderHoldsLock(filepath.Join(t.TempDir(), "nonexistent-proj")) {
		t.Fatal("missing lock directory reported as held")
	}
}

// TestEnsureAndDialScanFailureIsNotFatal: when /proc cannot be enumerated,
// the invisible-holder cleanup must NOT hard-error: it is treated as "no
// holders found", one bounded respawn is attempted, and the caller falls
// back with ok=false, err=nil (never a fatal error).
func TestEnsureAndDialScanFailureIsNotFatal(t *testing.T) {
	root := t.TempDir()

	// Hold the flock so EnsureAndDial enters the invisible-holder branch.
	// The probe does NOT create the .codegraph dir (missing dir = free), so
	// the test creates it.
	lockPath := filepath.Join(CodeGraphDir(root), "codegraph.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	origScan := scanProcsFn
	scanProcsFn = func() ([]procSnapshot, error) {
		return nil, errors.New("cannot enumerate /proc")
	}
	t.Cleanup(func() { scanProcsFn = origScan })

	spawnCalls := 0
	origSpawn := spawnDetachedFn
	spawnDetachedFn = func(string, *SpawnOpts) error {
		spawnCalls++
		return nil
	}
	t.Cleanup(func() { spawnDetachedFn = origSpawn })

	conn, br, hello, ok, err := EnsureAndDial(root, 200*time.Millisecond, 10*time.Millisecond, nil)
	if ok {
		t.Fatal("EnsureAndDial succeeded although the flock stayed held")
	}
	if err != nil {
		t.Fatalf("scan failure must not be fatal, got err=%v", err)
	}
	if spawnCalls != 2 {
		t.Fatalf("spawn calls = %d, want 2 (original + one bounded respawn)", spawnCalls)
	}
	if conn != nil || br != nil || hello.PID != 0 {
		t.Fatalf("unexpected outputs on fallback: conn=%v br=%v hello=%+v", conn, br, hello)
	}
}

// TestHelperHoldFlock is not a real test: it is re-executed as a subprocess
// by TestEnsureAndDialSelfHealsInvisibleHolder to hold the DB flock from a
// separate process. Activated by CG_HELPER_HOLD_FLOCK=1; the lock path comes
// from CG_HELPER_LOCK. SIGTERM (default disposition) terminates it, which
// releases the flock.
func TestHelperHoldFlock(t *testing.T) {
	if os.Getenv("CG_HELPER_HOLD_FLOCK") != "1" {
		t.Skip("helper only")
	}
	lockPath := os.Getenv("CG_HELPER_LOCK")
	if lockPath == "" {
		fmt.Fprintln(os.Stderr, "CG_HELPER_LOCK not set")
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		os.Exit(2)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(2)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(3)
	}
	fmt.Fprintln(os.Stdout, "HELD")
	time.Sleep(300 * time.Second)
}

package daemon

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestResolveDaemonBinaryPrefersExecutableWhenArgv0HitsStaleArtifact is the
// regression test for the MCP handshake failure: a client started with a
// relative argv[0] ("codegraph-go") from a workspace whose root contains a
// stale build artifact of the same name must still spawn the daemon from
// the real executable path — never from the stale artifact. Otherwise the
// daemon runs an old binary, the version check against the client fails,
// and handshake loops forever.
func TestResolveDaemonBinaryPrefersExecutableWhenArgv0HitsStaleArtifact(t *testing.T) {
	dir := t.TempDir()
	// The stale artifact the client could be confused by: a real file named
	// after the binary sitting in the workspace root.
	stale := filepath.Join(dir, "codegraph-go")
	if err := os.WriteFile(stale, []byte("stale build artifact"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	realInstall := "/root/.local/bin/codegraph-go"
	got := resolveDaemonBinaryFrom(realInstall, "codegraph-go")
	if got != realInstall {
		t.Fatalf("resolveDaemonBinaryFrom(%q, %q) = %q, want %q (stale artifact %q must not win)",
			realInstall, "codegraph-go", got, realInstall, stale)
	}
}

// TestResolveDaemonBinaryFallsBackToArgv0ForGoRun keeps the dev `go run`
// compatibility: when os.Executable() points into the temporary go build
// dir (which is deleted when the run exits), the path the process was
// invoked with is used instead.
func TestResolveDaemonBinaryFallsBackToArgv0ForGoRun(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codegraph-go")
	if err := os.WriteFile(bin, []byte("dev binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	execPath := "/tmp/go-build1234567890/b001/exe/codegraph-go"
	got := resolveDaemonBinaryFrom(execPath, bin)
	if got != bin {
		t.Fatalf("resolveDaemonBinaryFrom(%q, %q) = %q, want %q", execPath, bin, got, bin)
	}
}

// TestResolveDaemonBinaryGoRunMissingArgv0: in the go-run scenario a
// missing or directory argv[0] must not break resolution — keep the
// executable path.
func TestResolveDaemonBinaryGoRunMissingArgv0(t *testing.T) {
	execPath := "/tmp/go-build1234567890/b001/exe/codegraph-go"
	if got := resolveDaemonBinaryFrom(execPath, "no-such-binary"); got != execPath {
		t.Fatalf("resolveDaemonBinaryFrom with missing argv[0] = %q, want %q", got, execPath)
	}
	if got := resolveDaemonBinaryFrom(execPath, t.TempDir()); got != execPath {
		t.Fatalf("resolveDaemonBinaryFrom with directory argv[0] = %q, want %q", got, execPath)
	}
}

func TestIsGoRunTempPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/go-build1234567890/b001/exe/codegraph-go", true},          // Linux go run
		{"/var/folders/ab/cd/T/go-build999/b001/exe/codegraph-go", true}, // macOS TMPDIR
		{"/tmp/go-build1/b001/exe/codegraph-go.test", true},              // go test binary
		{"/root/.local/bin/codegraph-go", false},                         // real install
		{"/usr/local/bin/codegraph-go", false},                           // real install
		{"/root/codegraph-go/codegraph-go", false},                       // stale workspace artifact
		{"/home/u/tmp/go-build123/b001/exe/codegraph-go", true},          // custom TMPDIR
	}
	for _, c := range cases {
		if got := isGoRunTempPath(c.path); got != c.want {
			t.Errorf("isGoRunTempPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestSpawnDetachedSystemdRunSyncFallbackClearsQuota is the regression for
// the sync-rollback leak: when the systemd-run wrapper fails at cmd.Start()
// (ENOENT — the binary vanished from PATH mid-spawn, the classic case),
// SpawnDetached respawns the daemon directly. It must also clear quotaArgs,
// because the shared code path below decides whether to sit in the 150ms
// async scope-failure probe purely from quotaArgs != nil. Without the clear,
// the DIRECT child runs through that probe, and — the probe's second
// condition is "no live daemon holds the pidfile yet" — a direct daemon that
// fails to come up non-zero inside the window is spawned a SECOND time. The
// fake daemon appends one marker line per exec with its parent pid, so the
// test can count actual spawns: exactly one, and it must be a child of this
// test process (a direct exec, never the wrapper).
func TestSpawnDetachedSystemdRunSyncFallbackClearsQuota(t *testing.T) {
	root := t.TempDir()
	execLog := filepath.Join(root, "execs.log")

	// The "daemon": records every exec (pid of its spawner) and exits
	// non-zero — like a real spawn that fails to come up, which is exactly the
	// condition the async probe mistakes for a scope-start failure.
	self := filepath.Join(root, "fake-daemon.sh")
	body := "#!/bin/sh\nprintf 'exec %s %s\\n' \"$0\" \"$PPID\" >> '" + execLog + "'\nexit 3\n"
	if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// Wrapper that cannot be executed at all → cmd.Start() fails
	// synchronously → the direct-respawn branch runs.
	oldArgs := systemdRunArgsFn
	systemdRunArgsFn = func(string, []string, *SpawnOpts) []string {
		return []string{filepath.Join(root, "no-such-systemd-run")}
	}
	defer func() { systemdRunArgsFn = oldArgs }()

	oldResolve := resolveDaemonBinaryFn
	resolveDaemonBinaryFn = func() (string, error) { return self, nil }
	defer func() { resolveDaemonBinaryFn = oldResolve }()

	// lockedBuffer: the respawned child's early-exit watcher logs from a
	// goroutine while this test reads the buffer.
	var logBuf lockedBuffer
	defer captureSlog(&logBuf)()

	if err := SpawnDetached(root, &SpawnOpts{CPUQuotaPercent: 150}); err != nil {
		t.Fatalf("SpawnDetached must fall back to a direct spawn and succeed: %v", err)
	}
	if !strings.Contains(logBuf.String(), "systemd-run spawn failed, falling back to direct spawn") {
		t.Fatalf("expected the synchronous fallback warn line, got: %q", logBuf.String())
	}

	// Give the async probe window (150ms) plus the respawn it would trigger
	// time to happen, then count the execs: one direct spawn, never the
	// wrapper, every child parented by this test process.
	time.Sleep(500 * time.Millisecond)
	raw, err := os.ReadFile(execLog)
	if err != nil {
		t.Fatalf("daemon was never spawned: %v", err)
	}
	lines := strings.FieldsFunc(string(raw), func(r rune) bool { return r == '\n' })
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 direct daemon exec, got %d: %q — the cleared quotaArgs regressed (the direct child ran through the scope probe and respawned itself)", len(lines), string(raw))
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 3 || fields[0] != "exec" || filepath.Base(fields[1]) != "fake-daemon.sh" {
		t.Fatalf("unexpected exec record %q", lines[0])
	}
	if fields[2] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("daemon must be a direct child of this process (direct spawn), got ppid %s want %d", fields[2], os.Getpid())
	}
}

// TestDaemonAliveAtRejectsReusedPID: the pidfile liveness probe behind the
// systemd-run fallback window must not accept a live pid whose /proc start
// time differs from the recorded ProcStart — such a pid is a different
// incarnation (PID reuse) and treating it as a live daemon would wrongly
// suppress the direct-spawn fallback. daemonAliveAt reuses the same
// procStartTime mechanism as verifyDaemonIdentity.
func TestDaemonAliveAtRejectsReusedPID(t *testing.T) {
	start := procStartTime(os.Getpid())
	if start <= 0 {
		t.Skip("no /proc start time on this platform; the ProcStart guard is a no-op")
	}
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(procStart int64) {
		if err := os.WriteFile(pidPath, EncodeLock(LockInfo{
			PID:        os.Getpid(),
			Version:    PackageVersion,
			SocketPath: PreferredSocket(root),
			StartedAt:  1,
			ProcStart:  procStart,
		}), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(start)
	if !daemonAliveAt(root) {
		t.Fatal("a live pid recorded with its real ProcStart must report alive")
	}

	// Same live pid, forged start time: incarnation mismatch → the recorded
	// daemon is gone, the pid is someone else's recycled slot.
	write(1)
	if daemonAliveAt(root) {
		t.Fatal("a live pid with ProcStart=1 (mismatched incarnation) must report dead")
	}
}

// TestSystemdRunArgsGate: the quota scope wrapper only kicks in when
// CPUQuotaPercent > 0 AND the process is root AND systemd-run is usable on
// a systemd host; every other combination must return nil so the plain
// spawn path runs.
func TestSystemdRunArgsGate(t *testing.T) {
	self := "/root/.local/bin/codegraph-go"
	args := []string{"-workdir", "/root/workspace"}

	if got := systemdRunArgs(self, args, nil); got != nil {
		t.Fatalf("nil opts must not wrap: %v", got)
	}
	if got := systemdRunArgs(self, args, &SpawnOpts{}); got != nil {
		t.Fatalf("zero quota must not wrap: %v", got)
	}

	// Non-root must never wrap regardless of host capabilities: creating a
	// transient scope on the system manager requires euid 0.
	if got := systemdRunArgsFor(1000, self, args, &SpawnOpts{CPUQuotaPercent: 150}); got != nil {
		t.Fatalf("non-root euid must not wrap: %v", got)
	}

	got := systemdRunArgsFor(0, self, args, &SpawnOpts{CPUQuotaPercent: 150})
	if fi, err := os.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		if got != nil {
			t.Fatalf("non-systemd host must not wrap: %v", got)
		}
		t.Skip("no systemd on this host; argv shape covered on systemd hosts")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		if got != nil {
			t.Fatalf("systemd-run absent must not wrap: %v", got)
		}
		t.Skip("systemd-run not on PATH in this environment")
	}
	if got == nil {
		t.Fatal("systemd host + systemd-run + quota + root must produce argv")
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "systemd-run") || !strings.Contains(joined, "--quiet") ||
		!strings.Contains(joined, "--scope") || !strings.Contains(joined, "CPUQuota=150%") {
		t.Fatalf("unexpected systemd-run argv: %v", got)
	}
	// --no-ask-password keeps the wrapper non-interactive: a detached spawn
	// path must never block on an authorization prompt.
	hasNoAsk := false
	for _, a := range got {
		if a == "--no-ask-password" {
			hasNoAsk = true
			break
		}
	}
	if !hasNoAsk {
		t.Fatalf("systemd-run argv must carry --no-ask-password: %v", got)
	}
	// The daemon binary must follow the "--" separator verbatim.
	sep := -1
	for i, a := range got {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(got) || got[sep+1] != self {
		t.Fatalf("daemon binary must follow '--': %v", got)
	}
}

// TestSpawnDetachedSystemdRunAsyncFallback is the regression test for the
// async-start failure path: when the wrapped command (systemd-run) launches
// fine but exits non-zero almost immediately — the transient scope failed
// to be created — SpawnDetached must notice within its short window and
// fall back to a direct exec in the same call.
//
// self is replaced by /bin/false for the wrapped command and by a sleeper
// script for the direct fallback, via a fake systemdRunArgs; the pidfile
// check is exercised against a fresh temp root with no live daemon.
func TestSpawnDetachedSystemdRunAsyncFallback(t *testing.T) {
	if _, err := os.Stat("/bin/false"); err != nil {
		t.Skip("/bin/false unavailable")
	}
	root := t.TempDir()

	// The "daemon" we spawn is a tiny shell script that proves the fallback
	// ran by writing a marker and exiting — a real daemon would write a
	// pidfile; here the marker on disk is the signal (and the process
	// leaves nothing behind).
	marker := filepath.Join(root, "fallback-ran.marker")
	self := filepath.Join(root, "fake-daemon.sh")
	body := "#!/bin/sh\nprintf ran > '" + marker + "'\n"
	if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// Force the systemd-run path with a wrapper that exits non-zero
	// immediately — simulating an async scope-creation failure.
	oldArgs := systemdRunArgsFn
	systemdRunArgsFn = func(selfArg string, args []string, opts *SpawnOpts) []string {
		return []string{"/bin/false"}
	}
	defer func() { systemdRunArgsFn = oldArgs }()

	oldResolve := resolveDaemonBinaryFn
	resolveDaemonBinaryFn = func() (string, error) { return self, nil }
	defer func() { resolveDaemonBinaryFn = oldResolve }()

	// Capture slog so the fallback is asserted, not merely tolerated: without
	// the warn line the test could pass through the 150ms-still-alive path.
	// lockedBuffer for the same reason as above — respawn watchers log async.
	var logBuf lockedBuffer
	defer captureSlog(&logBuf)()

	err := SpawnDetached(root, &SpawnOpts{CPUQuotaPercent: 150})
	if err != nil {
		t.Fatalf("SpawnDetached must fall back and succeed: %v", err)
	}
	if !strings.Contains(logBuf.String(), "falling back to direct spawn") {
		t.Fatalf("expected async-failure fallback warn, log: %q", logBuf.String())
	}
	// The fallback spawn is asynchronous — poll for the marker the fake
	// daemon writes so the test asserts the fallback exec really happened
	// (not merely that SpawnDetached returned nil).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if fi, statErr := os.Stat(marker); statErr == nil && fi.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fallback marker %s not written within 2s — direct spawn never ran", marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSpawnDetachedOpenDaemonLogFails is the regression for the swallowed
// open failure: a .codegraph that cannot host daemon.log used to leave
// logFile=nil and spawn anyway, discarding the child's stdout/stderr — the
// parent then saw nothing but a dial timeout. SpawnDetached must now fail
// BEFORE spawning anything, with the phase, the target root, the log path
// and the errno in the error EnsureAndDial propagates.
func TestSpawnDetachedOpenDaemonLogFails(t *testing.T) {
	t.Run("daemon.log is a directory", func(t *testing.T) {
		root := t.TempDir()
		cgDir := CodeGraphDir(root)
		logPath := filepath.Join(cgDir, "daemon.log")
		if err := os.MkdirAll(logPath, 0o755); err != nil {
			t.Fatal(err)
		}

		// Must never be executed: the open failure precedes the spawn.
		marker := filepath.Join(root, "spawned.marker")
		self := filepath.Join(root, "fake-daemon.sh")
		body := "#!/bin/sh\nprintf ran > '" + marker + "'\n"
		if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		oldResolve := resolveDaemonBinaryFn
		resolveDaemonBinaryFn = func() (string, error) { return self, nil }
		defer func() { resolveDaemonBinaryFn = oldResolve }()

		err := SpawnDetached(root, nil)
		if err == nil {
			t.Fatal("SpawnDetached must fail when daemon.log cannot be opened, not spawn blind")
		}
		if !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("error must carry the open failure errno (EISDIR), got: %v", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, logPath) || !strings.Contains(msg, root) {
			t.Fatalf("error must name the log path and the target root, got: %v", err)
		}
		if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
			t.Fatalf("a child was spawned despite the log open failure (marker present: %v)", serr)
		}
	})

	t.Run("unwritable .codegraph", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: directory mode bits do not yield EACCES")
		}
		root := t.TempDir()
		cgDir := CodeGraphDir(root)
		if err := os.MkdirAll(cgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cgDir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(cgDir, 0o700) }()

		marker := filepath.Join(root, "spawned.marker")
		self := filepath.Join(root, "fake-daemon.sh")
		body := "#!/bin/sh\nprintf ran > '" + marker + "'\n"
		if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		oldResolve := resolveDaemonBinaryFn
		resolveDaemonBinaryFn = func() (string, error) { return self, nil }
		defer func() { resolveDaemonBinaryFn = oldResolve }()

		err := SpawnDetached(root, nil)
		if err == nil {
			t.Fatal("SpawnDetached must fail when daemon.log is not writable")
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("error must carry a permission errno, got: %v", err)
		}
		if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
			t.Fatalf("a child was spawned despite the log open failure (marker present: %v)", serr)
		}
	})
}

// captureSlog installs a thread-safe text handler writing into buf and
// returns the restore cleanup.
func captureSlog(buf *lockedBuffer) func() {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return func() { slog.SetDefault(old) }
}

// lockedBuffer is a strings.Builder with a mutex: the early-exit watcher
// logs from a goroutine while the test goroutine reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForLogContains(t *testing.T, buf *lockedBuffer, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return buf.String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return buf.String()
}

// fakeDaemonScript writes an executable shell script and points
// resolveDaemonBinaryFn at it for the duration of the test.
func fakeDaemonScript(t *testing.T, root, name, body string) string {
	t.Helper()
	self := filepath.Join(root, name)
	if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	oldResolve := resolveDaemonBinaryFn
	resolveDaemonBinaryFn = func() (string, error) { return self, nil }
	t.Cleanup(func() { resolveDaemonBinaryFn = oldResolve })
	return self
}

// exitWarnMsg is the diagnostic line emitted by watchDaemonExit when a
// spawned child dies and no live daemon holds the lock.
const exitWarnMsg = "spawned daemon exited; no live daemon holds the lock"

// TestSpawnDetachedExitWarnsOnceWithLogTail covers the diagnostic trail for
// the silent-death defect: a spawned child that exits non-zero while no
// live daemon holds the lock must leave exactly one warn record carrying
// the wait error, the exit code, the root, the log path and a bounded tail
// of daemon.log. The watcher is async (SpawnDetached still returns
// immediately), so the test polls the captured slog output; the single-
// watcher-per-spawn invariant is folded in by counting the message.
func TestSpawnDetachedExitWarnsOnceWithLogTail(t *testing.T) {
	root := t.TempDir()
	fakeDaemonScript(t, root, "fake-daemon.sh",
		"#!/bin/sh\necho 'TryAcquireLock: open daemon.pid.tmp: permission denied' >&2\nexit 3\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	if err := SpawnDetached(root, nil); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	waitForLogContains(t, &buf, exitWarnMsg, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	got := buf.String()
	if n := strings.Count(got, exitWarnMsg); n != 1 {
		t.Fatalf("expected exactly one exit warn (one watcher per spawn), got %d: %q", n, got)
	}
	for _, want := range []string{
		"exit status 3",
		"exit_code=3",
		"root=" + root,
		"log=" + logPath,
		"log_tail=",
		"permission denied",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("exit warn must contain %q, got: %q", want, got)
		}
	}
	// The child's own stderr must have reached daemon.log — evidence lives
	// on disk too, not only in the warn record.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("daemon.log must exist after a spawned child exits: %v", err)
	}
	if !strings.Contains(string(raw), "TryAcquireLock") {
		t.Fatalf("daemon.log must carry the child's stderr, got: %q", raw)
	}
}

// TestSpawnDetachedCleanExitStaysQuiet is the clean-exit misfire guard for
// the exit watcher: a lock-race loser exits 0 and the watcher must produce
// no warn, or a normal spawn would be misreported as a failure. It also
// pins the happy path: the child's stdout/stderr still land in daemon.log.
func TestSpawnDetachedCleanExitStaysQuiet(t *testing.T) {
	root := t.TempDir()
	fakeDaemonScript(t, root, "fake-daemon.sh",
		"#!/bin/sh\necho daemon-stdout-line\necho daemon-stderr-line >&2\nexit 0\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	if err := SpawnDetached(root, nil); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, _ := os.ReadFile(logPath)
		if strings.Contains(string(raw), "daemon-stderr-line") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child output never reached %s, got: %q", logPath, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The child has exited (its output is complete); give the watcher a
	// moment, then assert silence.
	time.Sleep(200 * time.Millisecond)
	if got := buf.String(); strings.Contains(got, "spawned daemon exited") {
		t.Fatalf("clean exit must not warn, got: %q", got)
	}
}

// TestSpawnDetachedExitWarnSuppressedByLiveHolder builds the second
// suppression condition for real instead of simulating it: the test
// acquires root's daemon lock itself (the pidfile records this live test
// process, so daemonAliveAt is genuinely true), then the spawned child
// exits non-zero — the exact moment the watcher fires. daemonAliveAt must
// see the real holder and suppress the warn; asserting the negated
// condition without a live holder would not exercise the guard.
func TestSpawnDetachedExitWarnSuppressedByLiveHolder(t *testing.T) {
	root := t.TempDir()
	res, err := TryAcquireLock(root)
	if err != nil {
		t.Fatalf("TryAcquireLock: %v", err)
	}
	if res.Kind != "acquired" {
		t.Fatalf("the test must hold the lock, got %+v", res)
	}
	defer func() { _ = os.Remove(res.PidPath) }()
	if !daemonAliveAt(root) {
		t.Fatal("sanity: the live holder must satisfy daemonAliveAt before the child exits")
	}

	fakeDaemonScript(t, root, "fake-daemon.sh", "#!/bin/sh\nexit 4\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	if err := SpawnDetached(root, nil); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	// The child exits immediately; give the watcher ample time to warn if
	// the live-holder suppression regressed.
	time.Sleep(500 * time.Millisecond)
	if got := buf.String(); strings.Contains(got, "spawned daemon exited") {
		t.Fatalf("a live lock holder must suppress the exit warn, got: %q", got)
	}
}

// TestSpawnDetachedScopeWindowMisfireGuards locks the two faces of the
// post-window watcher: a wrapper that exits 0 (lock-race loser shape) must
// stay quiet, and a wrapper that fails AFTER the window must warn without
// triggering any respawn.
func TestSpawnDetachedScopeWindowMisfireGuards(t *testing.T) {
	t.Run("clean exit inside window stays quiet", func(t *testing.T) {
		root := t.TempDir()
		fakeDaemonScript(t, root, "fake-daemon.sh",
			"#!/bin/sh\nprintf ran > '"+filepath.Join(root, "spawned.marker")+"'\n")
		oldArgs := systemdRunArgsFn
		systemdRunArgsFn = func(string, []string, *SpawnOpts) []string {
			return []string{"/bin/true"}
		}
		defer func() { systemdRunArgsFn = oldArgs }()

		var buf lockedBuffer
		defer captureSlog(&buf)()

		if err := SpawnDetached(root, &SpawnOpts{CPUQuotaPercent: 150}); err != nil {
			t.Fatalf("SpawnDetached: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
		if got := buf.String(); strings.Contains(got, "scope") || strings.Contains(got, "falling back") {
			t.Fatalf("a clean-exit wrapper must not trigger scope diagnostics, got: %q", got)
		}
		if _, serr := os.Stat(filepath.Join(root, "spawned.marker")); !os.IsNotExist(serr) {
			t.Fatalf("the daemon binary must never run under a wrapped spawn (marker present: %v)", serr)
		}
	})

	t.Run("non-zero exit after window warns once", func(t *testing.T) {
		root := t.TempDir()
		fakeDaemonScript(t, root, "fake-daemon.sh",
			"#!/bin/sh\nprintf ran > '"+filepath.Join(root, "spawned.marker")+"'\n")
		// Wrapper sleeps past the probe window, then fails — the async
		// scope-failure shape the post-window watcher exists for.
		wrapper := filepath.Join(root, "fake-wrapper.sh")
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nsleep 0.4\nexit 7\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		oldArgs := systemdRunArgsFn
		systemdRunArgsFn = func(string, []string, *SpawnOpts) []string {
			return []string{wrapper}
		}
		defer func() { systemdRunArgsFn = oldArgs }()

		var buf lockedBuffer
		defer captureSlog(&buf)()

		if err := SpawnDetached(root, &SpawnOpts{CPUQuotaPercent: 150}); err != nil {
			t.Fatalf("SpawnDetached: %v", err)
		}
		got := waitForLogContains(t, &buf, "exited with error after startup window", 3*time.Second)
		if !strings.Contains(got, "exit status 7") {
			t.Fatalf("post-window warn must carry the wait error, got: %q", got)
		}
		// Observability only: the failed scope must not respawn the daemon.
		time.Sleep(200 * time.Millisecond)
		if _, serr := os.Stat(filepath.Join(root, "spawned.marker")); !os.IsNotExist(serr) {
			t.Fatalf("a post-window scope failure must not respawn the daemon (marker present: %v)", serr)
		}
	})
}

// TestDaemonLogTailBoundaries exercises the composed helper end to end on
// real files: bounded read, truncated-first-line drop, scrub, and the
// final <= daemonLogTailBytes guarantee — never tail and scrub in isolation.
func TestDaemonLogTailBoundaries(t *testing.T) {
	openTail := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "daemon.log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return daemonLogTail(f)
	}

	t.Run("small complete file passes through", func(t *testing.T) {
		body := "line one\nline two\n"
		if got := openTail(t, body); got != body {
			t.Fatalf("a complete small file must be returned verbatim, got %q", got)
		}
	})

	t.Run("secret split across the read boundary does not leak", func(t *testing.T) {
		// The 4096-byte read window opens mid-value of token=<4096 x Z>:
		// scrub can only see 'ZZZ...' with no prefix to identify it. The
		// partial first line must be dropped wholesale — emitting even one
		// 'Z' leaks secret material.
		payload := "token=" + strings.Repeat("Z", daemonLogTailBytes)
		got := openTail(t, payload)
		if strings.Contains(got, "Z") {
			t.Fatalf("a boundary-split secret must not leak, got %d bytes starting %q", len(got), got[:min(64, len(got))])
		}
	})

	t.Run("boundary split only drops the first line", func(t *testing.T) {
		// Same shape plus one complete line after it: evidence is preserved
		// while the unverifiable fragment is still discarded.
		payload := "token=" + strings.Repeat("Z", daemonLogTailBytes) + "\nsurvivor-line\n"
		got := openTail(t, payload)
		if got != "survivor-line\n" {
			t.Fatalf("only the partial line may be dropped, got %q", got)
		}
	})

	t.Run("scrub expansion stays within the byte bound", func(t *testing.T) {
		// 7-byte 'pwd=x;\n' units expand into 17-byte 'pwd=[REDACTED];\n' —
		// raw tail + scrub used to blow past 4KiB; the post-scrub clamp must
		// hold while the masked content survives.
		payload := strings.Repeat("pwd=x;\n", 1500)
		got := openTail(t, payload)
		if len(got) > daemonLogTailBytes {
			t.Fatalf("scrubbed tail exceeded %d bytes: %d", daemonLogTailBytes, len(got))
		}
		if !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "=x;") {
			t.Fatalf("masked secrets expected inside the bound, got %q...%q",
				got[:min(40, len(got))], got[max(0, len(got)-40):])
		}
	})
}

// TestScrubDiagTextPinsDeclaredShapes gives each declared pattern a sample
// that matches ONLY that pattern — the previous "Authorization: Bearer
// ... token=..." blob was consumed whole by the first (key=value) pattern,
// so the bearer/JWT/GitHub assertions were vacuous. Every case asserts the
// marker lands next to the prefix its own pattern preserves.
func TestScrubDiagTextPinsDeclaredShapes(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		must   string // scrubbed form must contain this
		forbid string // scrubbed form must not contain this
	}{
		{"key=value", "open token=xyz7890123456789012345 failed", "token=[REDACTED]", "xyz7890123456789012345"},
		{"key colon", "passwd: hunter2x", "passwd: [REDACTED]", "hunter2x"},
		{"bearer no key context", "upstream returned Bearer abcdefghijklmno twice", "Bearer [REDACTED]", "abcdefghijklmno"},
		{"jwt", "saw eyJhbgvknqz3x9b1a2c3d4.e5f6g7hijklmn0pq2r3.t5u6v7w8x9y0a1b2z done", "[REDACTED]", "hbgvknqz3x9b1a2c3d4"},
		{"github classic", "leaked ghp_abcdefghijklmnopqrst done", "ghp_[REDACTED]", "abcdefghijklmnopqrst"},
		{"github fine-grained", "leaked " + "github_pat_" + "25twpnvgj4qovqvhxggeuezqhttkbzywtlrgeuenc" + " done", "github_pat_[REDACTED]", "25twpnvgj4qovqvhxgge"},
	}
	for _, c := range cases {
		got := scrubDiagText(c.in)
		if !strings.Contains(got, c.must) || strings.Contains(got, c.forbid) {
			t.Errorf("%s: scrubDiagText(%q) = %q, want it to contain %q and not %q",
				c.name, c.in, got, c.must, c.forbid)
		}
	}
	// Non-secret content passes through.
	if got := scrubDiagText("open daemon.pid.tmp: permission denied"); got != "open daemon.pid.tmp: permission denied" {
		t.Fatalf("plain text must pass through, got %q", got)
	}
}

// TestSpawnDetachedLogTailBoundToOpenedFile proves the tail is read through
// the fd that was opened for the child's stdout — not by re-opening the
// log path. After SpawnDetached opens daemon.log, the test replaces the
// path twice in ways that would redirect a path-based read (rename, then a
// symlink to a marked sentinel). A path reopen would either pick up the
// sentinel content or miss the child's output; the fd-bound tail must
// still deliver the child's own final line and none of the sentinel.
func TestSpawnDetachedLogTailBoundToOpenedFile(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("pre-spawn line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel.log")
	if err := os.WriteFile(sentinel, []byte("SENTINEL-UNRELATED-DAEMON-LOG\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The child sleeps past SpawnDetached's return so the path swap lands
	// deterministically between the open and the exit — the same window a
	// log rotator or a symlink flip would exploit.
	fakeDaemonScript(t, root, "fake-daemon.sh",
		"#!/bin/sh\nsleep 0.3\necho 'child-fatal: lock denied' >&2\nexit 9\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	if err := SpawnDetached(root, nil); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	// Rotation shape first: rename the live log aside, then symlink the
	// path at an unrelated sentinel file.
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, logPath); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	got := waitForLogContains(t, &buf, exitWarnMsg, 5*time.Second)
	if !strings.Contains(got, "child-fatal: lock denied") {
		t.Fatalf("the tail must carry the child's output from the pre-open fd, got: %q", got)
	}
	if strings.Contains(got, "SENTINEL-UNRELATED-DAEMON-LOG") {
		t.Fatalf("the tail must never follow a swapped symlink, got: %q", got)
	}
	if !strings.Contains(got, "exit_code=9") {
		t.Fatalf("the warn must still carry the exit facts, got: %q", got)
	}
}

// TestSpawnDetachedNonRegularLogSkipsTail: a FIFO planted at daemon.log
// must not stall the spawn or the watcher — a path-based O_RDONLY tail
// would block forever once the child (the only writer) exits, and the
// parent had already closed its write end on return. The tail must be
// omitted while the bounded exit facts still warn.
func TestSpawnDetachedNonRegularLogSkipsTail(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfifo", logPath).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unsupported: %v (%s)", err, out)
	}

	fakeDaemonScript(t, root, "fake-daemon.sh", "#!/bin/sh\nexit 6\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	done := make(chan error, 1)
	go func() { done <- SpawnDetached(root, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SpawnDetached on a FIFO log must not block or fail: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SpawnDetached blocked on the FIFO daemon.log")
	}

	got := waitForLogContains(t, &buf, exitWarnMsg, 5*time.Second)
	if !strings.Contains(got, "exit_code=6") || !strings.Contains(got, "root="+root) {
		t.Fatalf("the bounded exit facts must warn even without a tail, got: %q", got)
	}
	if strings.Contains(got, "log_tail=") {
		t.Fatalf("a non-regular log must produce no tail, got: %q", got)
	}
}

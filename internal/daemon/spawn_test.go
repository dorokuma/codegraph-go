package daemon

import (
	"bytes"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dorokuma/codegraph-go/internal/safelog"
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

// TestSpawnDetachedUnwritableCodeGraphFails keeps the independent non-root
// EACCES leg of the swallowed-open regression (the root-proof EISDIR leg
// lives with the other fail-closed cases in cgdir_guard_test.go:
// TestSpawnDetachedDaemonLogOpenFailureFailsClosed). A .codegraph that
// cannot host daemon.log must fail SpawnDetached BEFORE spawning anything,
// with the target root and a permission errno EnsureAndDial can surface.
// Under euid 0 chmod does not yield EACCES, so this case skips — the skip
// is not a substitute for acceptance, it only documents the environment.
func TestSpawnDetachedUnwritableCodeGraphFails(t *testing.T) {
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
// spawned child dies and no live daemon is recorded in the pidfile. It
// states exactly what daemonAliveAt observes — the pidfile — and nothing
// about flock holders without a pidfile (see invisibleHolderHoldsLock in
// proxy.go for that counter-example).
const exitWarnMsg = "spawned daemon exited; no live daemon recorded in pidfile"

// TestSpawnDetachedExitWarnsOnceWithLogTail covers the diagnostic trail for
// the silent-death defect: a spawned child that exits non-zero while no
// live daemon is recorded in the pidfile must leave exactly one warn record carrying
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

// spawnLikeProduction builds and starts a detached child exactly the way
// SpawnDetached's plain-spawn branch does — filtered env + daemon marker,
// own session via Setsid, stdout/stderr on the SAME open file description
// the tail is read through — so tests can drive the real watchDaemonExit
// and observe its completion through done instead of guessing with sleeps.
// The returned kill func bounds cleanup when the child could outlive the
// test; callers wait on done with their own timeout so a wedged child can
// never hang the suite.
func spawnLikeProduction(t *testing.T, root, scriptPath string) (done <-chan struct{}, kill func()) {
	t.Helper()
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, EnvDaemonInternal+"=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, EnvDaemonInternal+"=1")
	cmd := exec.Command(scriptPath, "-workdir", root)
	cmd.Env = env
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start child: %v", err)
	}
	d := make(chan struct{})
	go func() {
		watchDaemonExit(cmd, root, logFile, logPath)
		close(d)
	}()
	killed := false
	kill = func() {
		if killed {
			return
		}
		killed = true
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Errorf("watchDaemonExit did not return within 5s after kill")
		}
	}
	t.Cleanup(kill)
	return d, kill
}

// waitWatcher returns after the watcher finished (the child reaped, its fd
// closed, the warn decision made) or fails the test on timeout. Asserting
// log silence only AFTER done removes the nondeterminism the fixed sleeps
// carried: no warn can be emitted once watchDaemonExit has returned.
func waitWatcher(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchDaemonExit did not return within 10s")
	}
}

// TestWatchDaemonExitCleanExitStaysQuiet is the clean-exit misfire guard for
// the exit watcher: a lock-race loser exits 0 and the watcher must produce
// no warn, or a normal spawn would be misreported as a failure. The test
// runs the REAL watchDaemonExit over a production-shaped child and only
// asserts silence after the watcher itself returned — completion is proven
// by the done handshake, not by a sleep. It also pins the happy path: the
// child's stdout/stderr still land in daemon.log.
func TestWatchDaemonExitCleanExitStaysQuiet(t *testing.T) {
	root := t.TempDir()
	self := fakeDaemonScript(t, root, "fake-daemon.sh",
		"#!/bin/sh\necho daemon-stdout-line\necho daemon-stderr-line >&2\nexit 0\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	done, _ := spawnLikeProduction(t, root, self)
	waitWatcher(t, done)
	if got := buf.String(); strings.Contains(got, "spawned daemon exited") {
		t.Fatalf("clean exit must not warn, got: %q", got)
	}
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("daemon.log must exist after the child exits: %v", err)
	}
	if !strings.Contains(string(raw), "daemon-stderr-line") {
		t.Fatalf("child output must reach daemon.log, got: %q", raw)
	}
}

// TestWatchDaemonExitWarnSuppressedByLiveHolder builds the second
// suppression condition for real instead of simulating it: the test
// acquires root's daemon lock itself (the pidfile records this live test
// process, so daemonAliveAt is genuinely true), then the child exits
// non-zero — the exact moment the watcher would fire. daemonAliveAt must
// see the real pidfile holder and suppress the warn; asserting the negated
// condition without a live holder would not exercise the guard. Silence is
// asserted only after the watcher returned, via the done handshake.
func TestWatchDaemonExitWarnSuppressedByLiveHolder(t *testing.T) {
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

	self := fakeDaemonScript(t, root, "fake-daemon.sh", "#!/bin/sh\nexit 4\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	done, _ := spawnLikeProduction(t, root, self)
	waitWatcher(t, done)
	if got := buf.String(); strings.Contains(got, "spawned daemon exited") {
		t.Fatalf("a live pidfile holder must suppress the exit warn, got: %q", got)
	}
}

// TestWatchDaemonExitFifoPidfileDoesNotBlock is the regression for
// O1_PIDFILE_FIFO_WATCHER_BLOCKED: daemonAliveAt used to os.ReadFile the
// pidfile by PATH, so a FIFO leaf with no writer parked the watcher
// forever inside open(2) after the child was already reaped — the
// deferred logFile.Close() never ran and no diagnostic trail was left.
// The fix opens the leaf with O_NONBLOCK (the open returns immediately),
// then refuses the non-regular fd, degrading to "no live daemon
// recorded" and emitting the warn. The test drives the REAL
// watchDaemonExit over a production-shaped child: the done handshake
// proves the watcher returned (and with it logFile.Close() and the
// child's reaping), and the warn must still carry the exit facts and the
// bounded tail.
func TestWatchDaemonExitFifoPidfileDoesNotBlock(t *testing.T) {
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfifo", pidPath).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unsupported: %v (%s)", err, out)
	}

	self := fakeDaemonScript(t, root, "fake-daemon.sh",
		"#!/bin/sh\necho 'child-fatal: lock denied' >&2\nexit 6\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	done, _ := spawnLikeProduction(t, root, self)
	waitWatcher(t, done) // the child is reaped and logFile closed here — a
	// blocked pidfile open wedges this handshake forever
	got := buf.String()
	if n := strings.Count(got, exitWarnMsg); n != 1 {
		t.Fatalf("a writerless FIFO pidfile must degrade to 'no live daemon' and emit exactly one warn, got %d: %q", n, got)
	}
	for _, want := range []string{
		"exit status 6",
		"exit_code=6",
		"root=" + root,
		"log_tail=",
		"child-fatal: lock denied",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the warn must still carry %q, got: %q", want, got)
		}
	}
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("daemon.log must exist after the child exits: %v", err)
	}
	if !strings.Contains(string(raw), "child-fatal: lock denied") {
		t.Fatalf("the child's output must reach daemon.log, got: %q", raw)
	}
}

// TestWatchDaemonExitRejectsSymlinkPidfile pins the leaf-type guard of
// daemonAliveAt: a pidfile that is a symlink — even to a regular file
// carrying a syntactically valid LockInfo naming a LIVE process (this
// test process itself, with its real ProcStart) — must never be
// followed. The pre-fix os.ReadFile followed the link and decoded the
// forged record, so daemonAliveAt reported the out-of-project file's
// process as the live holder and wrongly suppressed the warn. The fix
// refuses the leaf at open() time (O_NOFOLLOW → ELOOP), the watcher
// treats it as "no live daemon recorded", and the warn is emitted.
func TestWatchDaemonExitRejectsSymlinkPidfile(t *testing.T) {
	root := t.TempDir()
	pidPath := PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// External target: a regular file outside the project holding a
	// LockInfo that decodes as alive (live pid + real ProcStart) — the
	// strongest forgery shape, rejected purely because the pidfile leaf
	// is a symlink.
	target := filepath.Join(t.TempDir(), "daemon.pid")
	if err := os.WriteFile(target, EncodeLock(LockInfo{
		PID:        os.Getpid(),
		Version:    PackageVersion,
		SocketPath: PreferredSocket(root),
		StartedAt:  1,
		ProcStart:  procStartTime(os.Getpid()),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, pidPath); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	self := fakeDaemonScript(t, root, "fake-daemon.sh",
		"#!/bin/sh\necho 'child-fatal: lock denied' >&2\nexit 5\n")

	var buf lockedBuffer
	defer captureSlog(&buf)()

	done, _ := spawnLikeProduction(t, root, self)
	waitWatcher(t, done)
	got := buf.String()
	if n := strings.Count(got, exitWarnMsg); n != 1 {
		t.Fatalf("a symlinked pidfile must not be followed (a forged live record must not suppress the warn): got %d warns: %q", n, got)
	}
	for _, want := range []string{
		"exit status 5",
		"exit_code=5",
		"log_tail=",
		"child-fatal: lock denied",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the warn must still carry %q, got: %q", want, got)
		}
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

	t.Run("post-scrub clamp", func(t *testing.T) {
		// The composed defect (CG_B2_TAIL_CONTEXT_LOST): the post-scrub byte
		// clamp used to cut mid-record, amputating the identifying prefix of
		// the first surviving record. Layout: record 1 carries 'Header
		// Basic ' + a long opaque value; the raw 4KiB read window opens
		// inside it, so daemonLogTail's first drop removes the 'Header
		// Basic' prefix — the value fragment is unmaskable and must be
		// dropped WHOLESALE when the post-scrub clamp cannot emit a
		// complete record, not left for the production safelog to ship
		// verbatim. Assertions run on what the REAL safelog.SetupLogger
		// pipeline emits, not on the helper in isolation.
		// The payload fits the raw read window (3614 < 4096): record 1 is
		// read in full, prefix included. The defect only manifests AFTER
		// scrubbing — the 100 masked pwd records expand the body past
		// 4096, and the old byte-level clamp cut ~518 bytes off the front,
		// landing inside record 1: the emitted tail then starts with a
		// bare fragment of the Basic value, the identifying 'Header
		// Basic ' prefix lost on the cutting-room floor.
		secretFragment := "Q" + strings.Repeat("Q", 2999)
		payload := "Header Basic " + secretFragment + "\n" + strings.Repeat("pwd=x\n", 100)
		path := filepath.Join(t.TempDir(), "daemon.log")
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		tail := daemonLogTail(f)

		// Redirect os.Stderr to a pipe BEFORE SetupLogger binds it; a
		// goroutine drains concurrently so the non-blocking writer can
		// never stall, and the default logger is restored before reading.
		// SetupLogger rebinds three pieces of global state and its cleanup
		// only drains the writer — it does NOT restore them: slog.Default,
		// and the standard log package's output/flags/prefix. Leaving the
		// rebound defaults in place would point slog/log at a closed
		// scrubbing writer for the rest of the package run, so save and
		// restore all of them here (deferred, so failure paths restore
		// too).
		oldSlog := slog.Default()
		oldLogOut, oldLogFlags, oldLogPrefix := log.Writer(), log.Flags(), log.Prefix()
		restoreLogGlobals := func() {
			slog.SetDefault(oldSlog)
			log.SetOutput(oldLogOut)
			log.SetFlags(oldLogFlags)
			log.SetPrefix(oldLogPrefix)
		}
		defer restoreLogGlobals()
		oldStderr := os.Stderr
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = pw
		// Idempotent fallback: if any path below panics before the
		// explicit restore runs, still hand back os.Stderr and close
		// pw (double close is a no-op error, ignored).
		defer func() {
			os.Stderr = oldStderr
			_ = pw.Close()
		}()
		var outBuf lockedBuffer
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			_, _ = io.Copy(&outBuf, pr)
		}()
		_, cleanup := safelog.SetupLogger("warn")
		slog.Warn("spawned daemon exited; no live daemon recorded in pidfile",
			"error", "exit status 1", "root", "/r", "log", "/r/daemon.log", "log_tail", tail)
		safelog.Flush(2 * time.Second)
		cleanup()
		restoreLogGlobals()
		os.Stderr = oldStderr
		_ = pw.Close()
		<-drainDone
		_ = pr.Close()

		emitted := outBuf.String()
		if len(emitted) == 0 {
			t.Fatal("CG_B2_TAIL_CONTEXT_LOST safelog emitted nothing")
		}
		if !utf8.ValidString(emitted) {
			t.Fatal("CG_B2_TAIL_CONTEXT_LOST emitted record is not valid UTF-8")
		}
		// The unmasked, prefix-less secret fragment must never reach the
		// sink: the old byte clamp emitted it verbatim.
		if strings.Contains(emitted, "QQQQ") {
			t.Fatalf("CG_B2_TAIL_CONTEXT_LOST unmaskable secret fragment reached the sink (len=%d)", len(emitted))
		}
		// The warn itself must still carry the bounded facts and a tail
		// that fits the bound and keeps whole-record boundaries: the first
		// emitted log line must start at a complete record.
		if len(tail) == 0 || len(tail) > daemonLogTailBytes {
			t.Fatalf("CG_B2_TAIL_CONTEXT_LOST tail length %d outside (0, %d]", len(tail), daemonLogTailBytes)
		}
		if !strings.HasSuffix(tail, "\n") {
			t.Fatalf("CG_B2_TAIL_CONTEXT_LOST tail ends mid-record (len=%d)", len(tail))
		}
		line, _, _ := strings.Cut(emitted, "\n")
		idx := strings.Index(line, "log_tail=")
		if idx < 0 {
			t.Fatalf("CG_B2_TAIL_CONTEXT_LOST log_tail attr missing (emitted len=%d)", len(emitted))
		}
		first := strings.TrimPrefix(line[idx+len("log_tail="):], "\"")
		if strings.HasPrefix(first, "pwd=") && !strings.HasPrefix(first, "pwd=[REDACTED]") {
			t.Fatalf("CG_B2_TAIL_CONTEXT_LOST first emitted record lost its mask")
		}
		if !utf8.ValidString(tail) {
			t.Fatal("CG_B2_TAIL_CONTEXT_LOST tail is not valid UTF-8")
		}
		// No plaintext secret may survive the composed path.
		if strings.Contains(emitted, "pwd=x") {
			t.Fatal("CG_B2_TAIL_CONTEXT_LOST plaintext key=value residue reached the sink")
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
// log path. A deterministic ready→swap→release handshake (instead of a
// fixed sleep) lands the path replacement exactly between the open and the
// child's exit — the same window a log rotator or a symlink flip would
// exploit. The child blocks on a release FIFO until the parent has
// swapped the path; a ready file proves the child is actually waiting
// before the parent proceeds, and the deferred release keeps every
// failure path from stranding a blocked child.
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
	ready := filepath.Join(root, "child-ready.marker")
	releaseFifo := filepath.Join(root, "release.fifo")
	if out, err := exec.Command("mkfifo", releaseFifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unsupported: %v (%s)", err, out)
	}

	// The child announces readiness, then blocks reading the release FIFO
	// until the parent's write unblocks it: the swap below is guaranteed to
	// happen while the child is still alive and before its final write.
	fakeDaemonScript(t, root, "fake-daemon.sh",
		"#!/bin/sh\ntouch '"+ready+"'\nread x < '"+releaseFifo+"'\necho 'child-fatal: lock denied' >&2\nexit 9\n")

	// O_RDWR so the parent's open never blocks; the child's blocking read
	// releases when the token lands. Cleanup releases the child on every
	// path after the spawn so no blocked child can outlive the test.
	rf, err := os.OpenFile(releaseFifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open release fifo: %v", err)
	}
	spawned := false
	released := false
	release := func() {
		if !spawned || released {
			return
		}
		released = true
		_, _ = rf.WriteString("go\n")
	}
	defer func() {
		release()
		_ = rf.Close()
	}()

	var buf lockedBuffer
	defer captureSlog(&buf)()

	if err := SpawnDetached(root, nil); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	spawned = true

	// Ready barrier: the child must be parked on the FIFO read before the
	// parent swaps the path — without it the swap could land before the
	// child's open and prove nothing about fd binding.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, serr := os.Stat(ready); serr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never reached the release barrier within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Rotation shape first: rename the live log aside, then symlink the
	// path at an unrelated sentinel file. A path reopen would either pick
	// up the sentinel content or miss the child's output.
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, logPath); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	// Release the child: it now writes its final line (through the fd it
	// inherited — still the original inode) and exits 9; the watcher reaps
	// it and emits the warn synchronously before the tail assertions.
	release()

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

// TestSpawnDetachedRejectsSymlinkAndNonRegularDaemonLog pins the leaf
// safety contract of openDaemonLog: anything at daemon.log that is not a
// regular file at the path itself — a symlink (live, oversized, or
// dangling) or a FIFO — must be refused BEFORE any child is spawned, so the
// child's stdout/stderr and the parent's diagnostic tail can never be
// redirected outside the project or wedged on a non-regular inode. The
// pre-fix code followed the link blindly (CG_B1_LEAF_ACCEPTED): it opened
// the sentinel as the log, rotated the link itself when the target was
// oversized, created the dangling link's target outside the project, and
// happily spawned with a FIFO as the log. A normal regular file remains a
// working control.
func TestSpawnDetachedRejectsSymlinkAndNonRegularDaemonLog(t *testing.T) {
	// plantLeaf returns the leaf setup + an external-integrity checker for
	// targets the leaf could have redirected writes into.
	cases := []struct {
		name      string
		errno     error // errors.Is check the refusal must satisfy (nil = any error ok)
		plantLeaf func(t *testing.T, logPath string) (checkExternal func(t *testing.T))
	}{
		{
			name:  "symlink to regular sentinel",
			errno: syscall.ELOOP,
			plantLeaf: func(t *testing.T, logPath string) func(t *testing.T) {
				t.Helper()
				sentinel := filepath.Join(t.TempDir(), "sentinel.log")
				content := []byte("SENTINEL-EXTERNAL-TARGET\n")
				if err := os.WriteFile(sentinel, content, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, logPath); err != nil {
					t.Skipf("symlinks not supported: %v", err)
				}
				return func(t *testing.T) {
					t.Helper()
					got, rerr := os.ReadFile(sentinel)
					if rerr != nil {
						t.Fatalf("sentinel must still exist: %v", rerr)
					}
					if !bytes.Equal(got, content) {
						t.Fatalf("CG_B1_LEAF_ACCEPTED sentinel modified: len=%d want=%d", len(got), len(content))
					}
				}
			},
		},
		{
			name:  "symlink to oversized target",
			errno: syscall.ELOOP,
			plantLeaf: func(t *testing.T, logPath string) func(t *testing.T) {
				t.Helper()
				// A >10MiB target exercises the rotation guard: a followed
				// Stat reports the target size and used to rotate the leaf.
				big := filepath.Join(t.TempDir(), "big.log")
				bf, err := os.Create(big)
				if err != nil {
					t.Fatal(err)
				}
				if err := bf.Truncate(11 * 1024 * 1024); err != nil {
					_ = bf.Close()
					t.Fatal(err)
				}
				if err := bf.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(big, logPath); err != nil {
					t.Skipf("symlinks not supported: %v", err)
				}
				return func(t *testing.T) {
					t.Helper()
					if fi, serr := os.Stat(big); serr != nil || fi.Size() != 11*1024*1024 {
						t.Fatalf("CG_B1_LEAF_ACCEPTED oversized target touched: size/err=%v", serr)
					}
				}
			},
		},
		{
			name:  "dangling symlink",
			errno: syscall.ELOOP,
			plantLeaf: func(t *testing.T, logPath string) func(t *testing.T) {
				t.Helper()
				outside := filepath.Join(t.TempDir(), "created-outside.log")
				if err := os.Symlink(outside, logPath); err != nil {
					t.Skipf("symlinks not supported: %v", err)
				}
				return func(t *testing.T) {
					t.Helper()
					if _, serr := os.Lstat(outside); !os.IsNotExist(serr) {
						t.Fatalf("CG_B1_LEAF_ACCEPTED dangling-link target created outside: %v", serr)
					}
				}
			},
		},
		{
			name: "fifo leaf",
			plantLeaf: func(t *testing.T, logPath string) func(t *testing.T) {
				t.Helper()
				if out, err := exec.Command("mkfifo", logPath).CombinedOutput(); err != nil {
					t.Skipf("mkfifo unsupported: %v (%s)", err, out)
				}
				return func(t *testing.T) { t.Helper() }
			},
		},
		{
			// B1 exception case: a leaf link resolving INSIDE .codegraph is
			// refused by the previous os.Root scoped open, but the earlier
			// open could follow the link and CREATE the missing relative
			// target before the post-open identity recheck refused — the
			// refused open still mutated the directory. O_NOFOLLOW makes the
			// kernel reject the leaf at open() time (ELOOP) before O_CREAT
			// can materialize rel-target; the decisive assertion is that the
			// target never appears.
			name:  "within_directory_dangling_symlink",
			errno: syscall.ELOOP,
			plantLeaf: func(t *testing.T, logPath string) func(t *testing.T) {
				t.Helper()
				// Relative, same-directory target that must NOT pre-exist.
				target := filepath.Join(filepath.Dir(logPath), "rel-target")
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Fatalf("rel-target must not pre-exist: %v", err)
				}
				if err := os.Symlink("rel-target", logPath); err != nil {
					t.Skipf("symlinks not supported: %v", err)
				}
				return func(t *testing.T) {
					t.Helper()
					if _, serr := os.Lstat(target); !os.IsNotExist(serr) {
						t.Fatalf("CG_B1_INTERNAL_TARGET_CREATED dangling-link target %s was created by the refused open: %v", target, serr)
					}
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
			if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
				t.Fatal(err)
			}
			checkExternal := c.plantLeaf(t, logPath)

			// Any spawn would write the marker; startup preparation is
			// observed through systemdRunArgsFn, which SpawnDetached only
			// reaches once the log was opened.
			marker := filepath.Join(root, "spawned.marker")
			self := filepath.Join(root, "fake-daemon.sh")
			body := "#!/bin/sh\nprintf ran > '" + marker + "'\n"
			if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			oldResolve := resolveDaemonBinaryFn
			resolveDaemonBinaryFn = func() (string, error) { return self, nil }
			defer func() { resolveDaemonBinaryFn = oldResolve }()
			var prepCalls atomic.Int64
			oldArgs := systemdRunArgsFn
			systemdRunArgsFn = func(string, []string, *SpawnOpts) []string {
				prepCalls.Add(1)
				return nil
			}
			defer func() { systemdRunArgsFn = oldArgs }()

			// Bounded: the refusal must return promptly (a FIFO leaf must not
			// block the open either), never hang the spawn path.
			type res struct{ err error }
			rc := make(chan res, 1)
			start := time.Now()
			go func() { rc <- res{SpawnDetached(root, nil)} }()
			var serr error
			select {
			case r := <-rc:
				serr = r.err
			case <-time.After(2 * time.Second):
				t.Fatalf("CG_B1_LEAF_ACCEPTED SpawnDetached blocked on the %s leaf >2s", c.name)
			}
			elapsed := time.Since(start)

			if serr == nil {
				t.Fatalf("CG_B1_LEAF_ACCEPTED SpawnDetached accepted a %s daemon.log and spawned blind", c.name)
			}
			if c.errno != nil && !errors.Is(serr, c.errno) {
				t.Fatalf("refusal must carry %v, got: %v", c.errno, serr)
			}
			if !strings.Contains(serr.Error(), logPath) || !strings.Contains(serr.Error(), root) {
				t.Fatalf("refusal must name the log path and the target root, got: %v", serr)
			}
			if elapsed > 2*time.Second {
				t.Fatalf("CG_B1_LEAF_ACCEPTED refusal path took %s >2s", elapsed)
			}
			if got := prepCalls.Load(); got != 0 {
				t.Fatalf("CG_B1_LEAF_ACCEPTED reached startup preparation %d time(s) after a %s leaf", got, c.name)
			}
			if _, merr := os.Stat(marker); !os.IsNotExist(merr) {
				t.Fatalf("CG_B1_LEAF_ACCEPTED a child was spawned past a %s leaf (marker present: %v)", c.name, merr)
			}
			checkExternal(t)
		})
	}

	t.Run("regular file control", func(t *testing.T) {
		root := t.TempDir()
		logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("pre-existing log line\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		marker := filepath.Join(root, "spawned.marker")
		fakeDaemonScript(t, root, "fake-daemon.sh",
			"#!/bin/sh\nprintf ran > '"+marker+"'\nexit 0\n")

		if err := SpawnDetached(root, nil); err != nil {
			t.Fatalf("SpawnDetached on a regular daemon.log must succeed: %v", err)
		}
		// The child exits immediately; bound the poll for its marker.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, serr := os.Stat(marker); serr == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the control child never ran within 5s")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

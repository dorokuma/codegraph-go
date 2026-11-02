package daemon

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

	var logBuf strings.Builder
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(oldLogger)

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
	var logBuf strings.Builder
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(oldLogger)

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

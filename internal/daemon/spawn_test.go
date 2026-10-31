package daemon

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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

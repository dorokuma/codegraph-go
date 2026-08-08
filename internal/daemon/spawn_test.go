package daemon

import (
	"os"
	"path/filepath"
	"testing"
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

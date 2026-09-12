package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCodegraphBinary compiles the cmd/codegraph-go binary so the
// fatal-exit paths can be exercised end to end: those paths call os.Exit,
// which no in-process test can survive. The module root is two levels above
// this package's directory.
func buildCodegraphBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "codegraph-go")
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(pkgDir))
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/codegraph-go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runInitFatal runs `codegraph-go init <root>` with a fake $HOME and an
// optional dedicated config (red-team setup: isolated environment, fresh
// config lookup) and returns the exit code and captured stderr.
func runInitFatal(t *testing.T, bin, home, config, root string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, "init", root)
	cmd.Dir = t.TempDir() // isolate the ./codegraph-config.yaml lookup
	env := append(os.Environ(), "HOME="+home)
	if config != "" {
		env = append(env, "CODEGRAPH_CONFIG="+config)
	}
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("init did not exit with a process status: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return code, stderr.String()
}

// TestFatalInitSymlinkStderrNonEmpty is the red-team reproduction: `init` on
// a project whose .codegraph is a symlink must exit 1 AND print the db.Open
// symlink-jail rejection on stderr. Before the flush-on-fatal fix, the
// log.Fatalf line sat in the non-blocking writer's buffer while os.Exit
// killed the drain goroutine, and the failure exited with 0 bytes on stderr.
func TestFatalInitSymlinkStderrNonEmpty(t *testing.T) {
	bin := buildCodegraphBinary(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	proj := filepath.Join(base, "proj")
	target := filepath.Join(base, "escape-target")
	for _, d := range []string{home, proj, target} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(proj, ".codegraph")); err != nil {
		t.Fatal(err)
	}
	// Dedicated config: workdirs = [proj], so the allowlist check passes and
	// the failure comes from the db symlink jail (the line that used to be
	// swallowed).
	cfg := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(cfg, []byte("workdirs:\n  - "+proj+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stderr := runInitFatal(t, bin, home, cfg, proj)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", code, stderr)
	}
	if stderr == "" {
		t.Fatal("stderr is empty: fatal log line dropped by the non-blocking writer")
	}
	if !strings.Contains(stderr, ".codegraph is a symlink") {
		t.Fatalf("stderr missing the symlink-jail rejection: %q", stderr)
	}
	if !strings.Contains(stderr, "init:") {
		t.Fatalf("stderr missing the init fatal prefix: %q", stderr)
	}
}

// TestFatalInitAllowlistRejectStderrNonEmpty: init on a workdir outside the
// authority roots must exit 1 with a non-empty stderr — runInit's rejection
// travels the same fatal path as the symlink case. Before the fix this was
// also 0 bytes.
func TestFatalInitAllowlistRejectStderrNonEmpty(t *testing.T) {
	bin := buildCodegraphBinary(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{home, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// No config file: the allowlist falls back to the fake $HOME, so the
	// sibling directory outside it is refused (fail-closed authority check).
	code, stderr := runInitFatal(t, bin, home, "", outside)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", code, stderr)
	}
	if stderr == "" {
		t.Fatal("stderr is empty: fatal log line dropped by the non-blocking writer")
	}
	if !strings.Contains(stderr, "outside the codegraph authority roots") {
		t.Fatalf("stderr missing the authority-root rejection: %q", stderr)
	}
}

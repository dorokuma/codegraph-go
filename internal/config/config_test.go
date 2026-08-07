package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateWorkdirsEqualRoot: a workdir equal to an allowed root passes.
func TestValidateWorkdirsEqualRoot(t *testing.T) {
	if err := ValidateWorkdirs([]string{"/root"}, []string{"/root"}); err != nil {
		t.Fatalf("workdir equal to allow root must pass: %v", err)
	}
}

// TestValidateWorkdirsSubdir: descendants of an allowed root pass, at any
// nesting depth.
func TestValidateWorkdirsSubdir(t *testing.T) {
	for _, wd := range []string{
		"/root/codegraph-go",
		"/root/a/b/c/d",
		"/root/.hidden",
	} {
		if err := ValidateWorkdirs([]string{wd}, []string{"/root"}); err != nil {
			t.Fatalf("workdir %s inside /root must pass: %v", wd, err)
		}
	}
}

// TestValidateWorkdirsSiblingPrefixRejected: /root-other shares the string
// prefix but is not a descendant — path-segment containment must reject it.
func TestValidateWorkdirsSiblingPrefixRejected(t *testing.T) {
	err := ValidateWorkdirs([]string{"/root-other"}, []string{"/root"})
	if err == nil {
		t.Fatal("sibling prefix /root-other must be rejected")
	}
	if !strings.Contains(err.Error(), "/root-other") {
		t.Fatalf("error must name the offending workdir: %v", err)
	}
}

// TestValidateWorkdirsOutsideRejected: /opt and /tmp are outside /root.
func TestValidateWorkdirsOutsideRejected(t *testing.T) {
	for _, wd := range []string{"/opt", "/opt/emby-intro-detect", "/tmp", "/var/lib"} {
		if err := ValidateWorkdirs([]string{wd}, []string{"/root"}); err == nil {
			t.Fatalf("workdir %s outside /root must be rejected", wd)
		}
	}
}

// TestValidateWorkdirsPartialViolation: with several workdirs the error names
// every offending one and the allowed roots.
func TestValidateWorkdirsPartialViolation(t *testing.T) {
	err := ValidateWorkdirs([]string{"/root/codegraph-go", "/opt/evil"}, []string{"/root"})
	if err == nil {
		t.Fatal("partial violation must be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/opt/evil") || !strings.Contains(msg, "/root") {
		t.Fatalf("error must name both the offender and the roots: %v", msg)
	}
	if !strings.Contains(msg, "refusing to start") {
		t.Fatalf("error must be actionable: %v", msg)
	}
}

// TestValidateWorkdirsEmptyAllowlistFailClosed: an empty allowlist (no
// config file AND no resolvable $HOME) rejects every workdir — fail closed,
// there is no loose mode anymore.
func TestValidateWorkdirsEmptyAllowlistFailClosed(t *testing.T) {
	for _, wd := range []string{"/root", "/root/codegraph-go", "/opt/emby-intro-detect", "/tmp", "/var"} {
		if err := ValidateWorkdirs([]string{wd}, nil); err == nil {
			t.Fatalf("nil allowlist must reject %s (fail closed)", wd)
		}
		if err := ValidateWorkdirs([]string{wd}, []string{}); err == nil {
			t.Fatalf("empty allowlist must reject %s (fail closed)", wd)
		}
	}
}

// TestValidateWorkdirsMultiRoot: each workdir is checked against the whole
// allowlist.
func TestValidateWorkdirsMultiRoot(t *testing.T) {
	allow := []string{"/root", "/opt"}
	if err := ValidateWorkdirs([]string{"/opt/emby-intro-detect", "/root/codegraph-go"}, allow); err != nil {
		t.Fatalf("workdirs inside any allow root must pass: %v", err)
	}
	if err := ValidateWorkdirs([]string{"/var/lib"}, allow); err == nil {
		t.Fatal("/var/lib outside both roots must be rejected")
	}
}

// TestValidateWorkdirsEmptyWorkdirs: an empty workdirs list has nothing to
// validate and passes.
func TestValidateWorkdirsEmptyWorkdirs(t *testing.T) {
	if err := ValidateWorkdirs(nil, []string{"/root"}); err != nil {
		t.Fatalf("empty workdirs must pass: %v", err)
	}
	if err := ValidateWorkdirs([]string{}, []string{"/root"}); err != nil {
		t.Fatalf("empty workdirs must pass: %v", err)
	}
}

// TestValidateWorkdirsRelativePath: relative workdirs are canonicalized
// against the process cwd before comparison.
func TestValidateWorkdirsRelativePath(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkdirs([]string{"."}, []string{cwd}); err != nil {
		t.Fatalf("relative cwd against absolute allow root must pass: %v", err)
	}
}

// TestValidateWorkdirsSymlinkEscape: a symlink under the allow root pointing
// outside it must be rejected (canonical comparison), while a symlink
// resolving inside the root passes.
func TestValidateWorkdirsSymlinkEscape(t *testing.T) {
	root := t.TempDir()    // the allow root
	outside := t.TempDir() // a real directory outside the root

	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := ValidateWorkdirs([]string{escape}, []string{root}); err == nil {
		t.Fatalf("symlink escape %s -> %s must be rejected", escape, outside)
	}

	// A symlink whose target stays inside the root is fine.
	inside := filepath.Join(root, "real")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(root, "ok")
	if err := os.Symlink(inside, ok); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkdirs([]string{ok}, []string{root}); err != nil {
		t.Fatalf("symlink resolving inside the root must pass: %v", err)
	}
}

// TestValidateWorkdirsSymlinkRoot: the allow root itself is canonicalized
// too, so a symlinked root matches its real path.
func TestValidateWorkdirsSymlinkRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := ValidateWorkdirs([]string{real}, []string{link}); err != nil {
		t.Fatalf("workdir under real path of a symlinked allow root must pass: %v", err)
	}
}

// TestConfigPathEnv: $CODEGRAPH_CONFIG wins the lookup and is returned even
// when the file does not exist (LoadConfig reports the read error itself).
func TestConfigPathEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	t.Setenv("CODEGRAPH_CONFIG", p)
	if got := ConfigPath(); got != p {
		t.Fatalf("ConfigPath() = %q, want %q", got, p)
	}
}

// TestWorkdirAllowlist: a real config file is authoritative; missing,
// unparsable or workdir-less files fall back to $HOME.
func TestWorkdirAllowlist(t *testing.T) {
	dir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	homeC := canonical(home)

	good := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(good, []byte("workdirs:\n  - /root\n  - /opt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_CONFIG", good)
	got := WorkdirAllowlist("")
	if len(got) != 2 || got[0] != "/root" || got[1] != "/opt" {
		t.Fatalf("WorkdirAllowlist = %v, want [/root /opt] (config is authoritative, not $HOME=%s)", got, homeC)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("workdirs: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := WorkdirAllowlist(bad); len(got) != 1 || got[0] != homeC {
		t.Fatalf("unparsable config must fall back to $HOME, got %v", got)
	}
	if got := WorkdirAllowlist(filepath.Join(dir, "missing.yaml")); len(got) != 1 || got[0] != homeC {
		t.Fatalf("missing config must fall back to $HOME, got %v", got)
	}

	// Empty workdirs list in a real file: no roots declared → $HOME fallback.
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, []byte("workdirs: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := WorkdirAllowlist(empty); len(got) != 1 || got[0] != homeC {
		t.Fatalf("empty workdirs must fall back to $HOME, got %v", got)
	}
}

// TestWorkdirAllowlistHomeFallback: with no config file the allowlist
// defaults to $HOME — workdirs inside $HOME pass, outside are rejected.
func TestWorkdirAllowlistHomeFallback(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no $HOME: %v", err)
	}
	if insideRoot("/opt", home) {
		t.Skipf("$HOME %s contains /opt; outside-$HOME case meaningless here", home)
	}
	// Point the lookup at a non-existent file: no usable config anywhere.
	t.Setenv("CODEGRAPH_CONFIG", filepath.Join(t.TempDir(), "no-such-config.yaml"))

	allow := WorkdirAllowlist("")
	want := canonical(home)
	if len(allow) != 1 || allow[0] != want {
		t.Fatalf("WorkdirAllowlist = %v, want [%s] ($HOME fallback)", allow, want)
	}
	// A workdir inside $HOME passes.
	if err := ValidateWorkdirs([]string{filepath.Join(home, "codegraph-go")}, allow); err != nil {
		t.Fatalf("workdir inside $HOME must pass: %v", err)
	}
	// A workdir outside $HOME is rejected.
	if err := ValidateWorkdirs([]string{"/opt/emby-intro-detect"}, allow); err == nil {
		t.Fatal("workdir outside $HOME must be rejected with the $HOME fallback allowlist")
	}
}

// TestWorkdirAllowlistHomeFallbackUnresolvable: when $HOME itself cannot be
// resolved the allowlist is empty and validation is fail-closed. Skipped when
// the environment still resolves a home directory (e.g. via /etc/passwd).
func TestWorkdirAllowlistHomeFallbackUnresolvable(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CODEGRAPH_CONFIG", filepath.Join(t.TempDir(), "no-such-config.yaml"))
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("environment resolves a home directory despite empty $HOME")
	}
	allow := WorkdirAllowlist("")
	if len(allow) != 0 {
		t.Fatalf("unresolvable $HOME must yield empty allowlist, got %v", allow)
	}
	if err := ValidateWorkdirs([]string{"/root"}, allow); err == nil {
		t.Fatal("empty allowlist from unresolvable $HOME must reject every workdir (fail closed)")
	}
}

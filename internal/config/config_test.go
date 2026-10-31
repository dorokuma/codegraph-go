package config

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// TestConfigPathEnv: $CODEGRAPH_CONFIG wins the lookup when the file exists;
// a nonexistent path is skipped (L1) and the lookup falls through to the
// remaining candidates.
func TestConfigPathEnv(t *testing.T) {
	dir := t.TempDir()
	// Isolate $HOME so the ~/.config/codegraph/config.yaml candidate cannot
	// exist in this test (this machine has a real one).
	t.Setenv("HOME", filepath.Join(dir, "isolated-home"))
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("workdirs: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_CONFIG", good)
	if got := ConfigPath(); got != good {
		t.Fatalf("ConfigPath() = %q, want %q (existing $CODEGRAPH_CONFIG wins)", got, good)
	}

	// Nonexistent $CODEGRAPH_CONFIG is skipped: with no local/global config
	// the lookup returns "" instead of a dead path.
	missing := filepath.Join(dir, "does-not-exist.yaml")
	t.Setenv("CODEGRAPH_CONFIG", missing)
	if got := ConfigPath(); got != "" {
		t.Fatalf("ConfigPath() = %q, want \"\" when $CODEGRAPH_CONFIG points at a missing file", got)
	}

	// The next candidate in the priority order is used instead.
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "codegraph-config.yaml")
	if err := os.WriteFile(local, []byte("workdirs: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ConfigPath(); got != "./codegraph-config.yaml" {
		t.Fatalf("ConfigPath() = %q, want ./codegraph-config.yaml after skipping missing $CODEGRAPH_CONFIG", got)
	}
}

// TestExpandPath: ~ and $VAR expansion in config-supplied workdirs (L2).
func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no $HOME: %v", err)
	}
	if got := expandPath("~/proj"); got != filepath.Join(home, "proj") {
		t.Fatalf("expandPath(~/proj) = %q, want %q", got, filepath.Join(home, "proj"))
	}
	if got := expandPath("~"); got != home {
		t.Fatalf("expandPath(~) = %q, want %q", got, home)
	}
	t.Setenv("CODEGRAPH_TEST_VAR", "/opt/x")
	if got := expandPath("$CODEGRAPH_TEST_VAR/proj"); got != "/opt/x/proj" {
		t.Fatalf("expandPath($VAR/proj) = %q, want /opt/x/proj", got)
	}
	if got := expandPath("/plain/path"); got != "/plain/path" {
		t.Fatalf("expandPath(plain) = %q, want /plain/path", got)
	}
}

// TestWorkdirAllowlistTilde: config workdirs written with ~ are expanded
// before they become authority roots (L2), so `workdirs: [~/proj]` matches
// the expanded workdir instead of the literal tilde path.
func TestWorkdirAllowlistTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no $HOME: %v", err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("workdirs:\n  - ~/cg-allow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := WorkdirAllowlist(cfg)
	want := canonical(filepath.Join(home, "cg-allow"))
	if len(got) != 1 || got[0] != want {
		t.Fatalf("WorkdirAllowlist = %v, want [%s]", got, want)
	}
	// The expanded root must accept the expanded workdir (validation
	// round-trip).
	if err := ValidateWorkdirs([]string{filepath.Join(home, "cg-allow")}, got); err != nil {
		t.Fatalf("expanded workdir rejected by expanded allowlist: %v", err)
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
	// Host-env isolation: redirect $HOME into a fresh temp dir so the third
	// ConfigPath candidate (~/.config/codegraph/config.yaml) cannot resolve to
	// a host-global file. This machine ships one with workdirs:
	// [/root/workspace], which would otherwise be read as the authoritative
	// config and win over the $HOME fallback this test asserts. On Linux
	// os.UserHomeDir() reads $HOME, so the redirect also becomes the $HOME the
	// fallback must return below. The lookup hardcodes $HOME/.config (it does
	// not consult $XDG_CONFIG_HOME), so isolating $HOME alone is sufficient.
	t.Setenv("HOME", t.TempDir())

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

// TestYamlConfigParsesNewFields: the new optional resource/exclusion keys
// decode into the yamlConfig struct; unknown keys remain ignored.
func TestYamlConfigParsesNewFields(t *testing.T) {
	src := `
workdirs:
  - /root/workspace
exclude_dirs:
  - "*-backup*"
  - vendor2/
nice: 10
max_procs: 4
index_workers: 3
cpu_quota_percent: 150
unknown_future_key: whatever
`
	var yc yamlConfig
	if err := yaml.Unmarshal([]byte(src), &yc); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(yc.ExcludeDirs, []string{"*-backup*", "vendor2/"}) {
		t.Fatalf("ExcludeDirs = %v", yc.ExcludeDirs)
	}
	if yc.Nice != 10 || yc.MaxProcs != 4 || yc.IndexWorkers != 3 || yc.CPUQuotaPercent != 150 {
		t.Fatalf("numeric fields = %+v", yc)
	}
}

// TestApplyEnvOverridesClamp: out-of-range YAML values are clamped and the
// CODEGRAPH_* env vars override (ExcludeDirs appends).
func TestApplyEnvOverridesClamp(t *testing.T) {
	cfg := &Config{Nice: 99, MaxProcs: -5, CPUQuotaPercent: -3, ExcludeDirs: []string{"a"}}
	t.Setenv(EnvExcludeDirs, " b ,c/ , , ")
	t.Setenv(EnvNice, "-4")
	applyEnvOverrides(cfg)
	if cfg.Nice != 0 { // env override -4 clamped to 0
		t.Fatalf("Nice = %d, want 0", cfg.Nice)
	}
	if cfg.MaxProcs != 0 || cfg.CPUQuotaPercent != 0 {
		t.Fatalf("negative caps must clamp to 0, got %+v", cfg)
	}
	want := []string{"a", "b", "c/"}
	if !reflect.DeepEqual(cfg.ExcludeDirs, want) {
		t.Fatalf("ExcludeDirs = %v, want %v", cfg.ExcludeDirs, want)
	}

	// max_procs env override wins and clamps to 64.
	t.Setenv(EnvMaxProcs, "999")
	applyEnvOverrides(cfg)
	if cfg.MaxProcs != 64 {
		t.Fatalf("MaxProcs = %d, want 64", cfg.MaxProcs)
	}

	// cpu_quota_percent clamps to [0,6400]: 6400 = a 64-core host fully
	// allotted (CPUQuota=<N>% with 100 per core); anything higher is
	// meaningless to systemd and must not leak through.
	t.Setenv(EnvCPUQuotaPercent, "99999")
	applyEnvOverrides(cfg)
	if cfg.CPUQuotaPercent != 6400 {
		t.Fatalf("CPUQuotaPercent = %d, want 6400", cfg.CPUQuotaPercent)
	}
}

// TestLoadConfigPublishesRuntimeSnapshot: LoadConfig stores exclude_dirs and
// index_workers in the package-level snapshot read by ExcludeDirs() and
// IndexWorkers().
func TestLoadConfigPublishesRuntimeSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "isolated-home"))
	cfgFile := filepath.Join(dir, "config.yaml")
	body := "workdirs:\n  - " + dir + "\nexclude_dirs:\n  - junk\nindex_workers: 3\nnice: 5\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_CONFIG", cfgFile)
	t.Cleanup(func() { SetRuntimeConfigForTesting(nil, 0) })

	// Isolate flag state: LoadConfig parses os.Args.
	oldArgs := os.Args
	oldFlagCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{os.Args[0]}
	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlagCommandLine
	}()

	cfg := LoadConfig()
	if cfg.Nice != 5 {
		t.Fatalf("cfg.Nice = %d, want 5", cfg.Nice)
	}
	if got := ExcludeDirs(); !reflect.DeepEqual(got, []string{"junk"}) {
		t.Fatalf("ExcludeDirs() = %v, want [junk]", got)
	}
	if got := IndexWorkers(); got != 3 {
		t.Fatalf("IndexWorkers() = %d, want 3 (config wins over default)", got)
	}
	// Env override still wins over the config snapshot.
	t.Setenv(EnvIndexWorkers, "2")
	if got := IndexWorkers(); got != 2 {
		t.Fatalf("IndexWorkers() = %d, want 2 (env beats config)", got)
	}
}

// TestLoadConfigAbsolutizesConfigFile: a relative config path (the
// ./codegraph-config.yaml lookup candidate) must be stored back into
// cfg.ConfigFile as an absolute path so a detached daemon whose cmd.Dir
// differs still opens the same file (S3).
func TestLoadConfigAbsolutizesConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "isolated-home"))
	local := filepath.Join(dir, "codegraph-config.yaml")
	body := "workdirs:\n  - " + dir + "\n"
	if err := os.WriteFile(local, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	oldFlagCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{os.Args[0]}
	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlagCommandLine
	}()

	cfg := LoadConfig()
	if !filepath.IsAbs(cfg.ConfigFile) {
		t.Fatalf("ConfigFile = %q, want absolute path", cfg.ConfigFile)
	}
	if cfg.ConfigFile != local {
		t.Fatalf("ConfigFile = %q, want %q", cfg.ConfigFile, local)
	}
}

// TestLoadConfigExplicitWorkdir: ExplicitWorkdir records the -workdir flag
// value (post expandPath) and stays "" when the flag was omitted — even
// after Workdir is backfilled from Workdirs[0]. Downstream narrowing
// (extraction.NarrowExcludedWorkdirs) relies on this distinction.
func TestLoadConfigExplicitWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "isolated-home"))
	cfgFile := filepath.Join(dir, "config.yaml")
	body := "workdirs:\n  - " + dir + "\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_CONFIG", cfgFile)

	oldArgs := os.Args
	oldFlagCommandLine := flag.CommandLine
	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlagCommandLine
	}()

	// No -workdir: ExplicitWorkdir stays empty even though Workdir is
	// backfilled from the config workdirs.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{os.Args[0]}
	cfg := LoadConfig()
	if cfg.ExplicitWorkdir != "" {
		t.Fatalf("ExplicitWorkdir = %q, want \"\" without -workdir", cfg.ExplicitWorkdir)
	}
	if cfg.Workdir != dir {
		t.Fatalf("Workdir = %q, want %q (backfilled from config)", cfg.Workdir, dir)
	}

	// Explicit -workdir: recorded in ExplicitWorkdir.
	explicit := filepath.Join(dir, "wt")
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{os.Args[0], "-workdir", explicit}
	cfg = LoadConfig()
	if cfg.ExplicitWorkdir != explicit {
		t.Fatalf("ExplicitWorkdir = %q, want %q", cfg.ExplicitWorkdir, explicit)
	}
	if cfg.Workdirs[0] != explicit {
		t.Fatalf("Workdirs[0] = %q, want %q", cfg.Workdirs[0], explicit)
	}
}

// Package config provides centralised configuration for codegraph-go.
// It must not import any internal/business packages — only the standard library.
package config

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Env vars for extraction helpers (moved here so extraction packages don't
// read env vars directly).
const (
	EnvIndexWorkers    = "CODEGRAPH_INDEX_WORKERS"
	EnvHomeIndexAll    = "CODEGRAPH_GO_HOME_INDEX_ALL"
	EnvLogLevel        = "CODEGRAPH_LOG_LEVEL"
	EnvExcludeDirs     = "CODEGRAPH_EXCLUDE_DIRS"
	EnvNice            = "CODEGRAPH_NICE"
	EnvMaxProcs        = "CODEGRAPH_MAX_PROCS"
	EnvCPUQuotaPercent = "CODEGRAPH_CPU_QUOTA_PERCENT"
)

// Default log level when the env var is not set.
const DefaultLogLevel = "info"

// Config holds the top-level application configuration.
type Config struct {
	Workdir  string
	Workdirs []string
	// ExplicitWorkdir records the -workdir flag value the user actually
	// passed (post expandPath). It stays "" when -workdir was omitted,
	// even after the Workdir = Workdirs[0] backfill below — callers that
	// need to distinguish "user explicitly pointed at X" from "cwd
	// fallback / config workdir" (e.g. extraction.NarrowExcludedWorkdirs)
	// must read this field, not Workdir.
	ExplicitWorkdir string
	ConfigFile      string
	NoSync          bool
	// WarmDaemon records the -warm-daemon flag: the process only pre-warms
	// the shared daemon (probe → SpawnDetached → dial, exactly the path a
	// normal client takes, CPUQuota wrapper included) and exits. deploy.sh
	// uses it so the daemon is never started through a second, divergent
	// shell-side entry point.
	WarmDaemon bool
	LogLevel   string

	// Optional pruning / resource-limit knobs. Every field is backward
	// compatible: zero values reproduce the previous behaviour.
	ExcludeDirs     []string // extra dir names/globs/workdir-relative paths to skip
	Nice            int      // process nice level, clamped to [0,19]; 0 = untouched
	MaxProcs        int      // GOMAXPROCS cap, clamped to [0,64]; 0 = unlimited
	IndexWorkers    int      // parallel extract workers; 0 = built-in default
	CPUQuotaPercent int      // systemd-run scope CPUQuota percent; 0 = off
}

// yamlConfig mirrors the optional YAML config file schema. yaml.v3 ignores
// unknown keys, so newly added keys stay backward compatible with older
// binaries reading newer configs (and vice versa).
type yamlConfig struct {
	Workdirs        []string `yaml:"workdirs"`
	ExcludeDirs     []string `yaml:"exclude_dirs"`
	Nice            int      `yaml:"nice"`
	MaxProcs        int      `yaml:"max_procs"`
	IndexWorkers    int      `yaml:"index_workers"`
	CPUQuotaPercent int      `yaml:"cpu_quota_percent"`
}

// workdirsOnlyConfig is the MINIMAL read view WorkdirAllowlist uses: the
// single "workdirs" key and nothing else.
//
// Why a second struct: the authority roots are a security boundary, so
// decoding them must not depend on any other key in the file. Unmarshalling
// the full yamlConfig made a typo in an unrelated, non-privileged knob
// ("nice: soon", "index_workers: many") fail the whole document decode, and
// WorkdirAllowlist then fell back to $HOME — silently WIDENING the allowlist
// from the operator's narrow "workdirs: [/root/workspace]" to the entire
// home directory. Decoding only workdirs keeps the boundary intact: such a
// typo costs the resource knobs (LoadConfig already logs and ignores the
// file), never the authority roots.
type workdirsOnlyConfig struct {
	Workdirs []string `yaml:"workdirs"`
}

// LoadConfig parses CLI flags, reads the environment and the optional
// standalone YAML config, and returns a Config with defaults applied.
// Config file priority: -config flag > $CODEGRAPH_CONFIG >
// ./codegraph-config.yaml > ~/.config/codegraph/config.yaml.
// The -workdir flag, when set, is prepended to Workdirs. If Workdirs is
// still empty after all sources, the current working directory is used as
// the sole workdir.
func LoadConfig() Config {
	var cfg Config

	flag.StringVar(&cfg.Workdir, "workdir", "", "workspace root (default: cwd; prepended to config workdirs)")
	flag.StringVar(&cfg.ConfigFile, "config", "", "path to YAML config file")
	flag.BoolVar(&cfg.NoSync, "no-sync", false, "disable auto-sync file watcher")
	flag.BoolVar(&cfg.WarmDaemon, "warm-daemon", false, "pre-warm the shared daemon through the normal client spawn path, then exit")
	flag.Parse()

	// Determine config file path: -config flag > ConfigPath() lookup order.
	configPath := cfg.ConfigFile
	if configPath == "" {
		configPath = ConfigPath()
	}
	if configPath != "" {
		// Write back the absolute path so a detached daemon that changes
		// cmd.Dir still resolves the same file (SpawnDetached passes
		// -config verbatim).
		if abs, err := filepath.Abs(configPath); err == nil {
			configPath = abs
		}
		cfg.ConfigFile = configPath
	}

	// Read YAML config file.
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			log.Printf("config: read %s: %v", configPath, err)
		} else {
			var yc yamlConfig
			if err := yaml.Unmarshal(data, &yc); err != nil {
				log.Printf("config: parse %s: %v (ignoring file)", configPath, err)
			} else {
				if len(yc.Workdirs) > 0 {
					// L2: expand ~ and $VAR in config workdirs so `~/proj` and
					// $PROJECT_ROOT resolve the way users expect.
					cfg.Workdirs = make([]string, 0, len(yc.Workdirs))
					for _, wd := range yc.Workdirs {
						if wd = expandPath(wd); wd != "" {
							cfg.Workdirs = append(cfg.Workdirs, wd)
						}
					}
				}
				cfg.ExcludeDirs = yc.ExcludeDirs
				cfg.Nice = yc.Nice
				cfg.MaxProcs = yc.MaxProcs
				cfg.IndexWorkers = yc.IndexWorkers
				cfg.CPUQuotaPercent = yc.CPUQuotaPercent
			}
		}
	}

	// -workdir flag overrides: prepend to the list if not already present.
	// The expanded value is also kept in ExplicitWorkdir so downstream
	// narrowing can tell an explicit target apart from the cwd fallback.
	if cfg.Workdir != "" {
		cfg.Workdir = expandPath(cfg.Workdir)
		cfg.ExplicitWorkdir = cfg.Workdir
		found := false
		for _, wd := range cfg.Workdirs {
			if wd == cfg.Workdir {
				found = true
				break
			}
		}
		if !found {
			cfg.Workdirs = append([]string{cfg.Workdir}, cfg.Workdirs...)
		}
	}

	// Fallback to single cwd workdir.
	if len(cfg.Workdirs) == 0 {
		if wd, err := os.Getwd(); err == nil {
			cfg.Workdirs = []string{wd}
		}
	}

	// Primary workdir for backward compatibility.
	if len(cfg.Workdirs) > 0 {
		cfg.Workdir = cfg.Workdirs[0]
	}

	// CODEGRAPH_* env vars override/extend the YAML fields, then the numeric
	// ranges are clamped so a typo can never leave a harmful value.
	applyEnvOverrides(&cfg)
	// Publish the cross-package snapshot (extraction skip rules and the
	// index worker pool read these without a Config parameter).
	storeRuntimeConfig(cfg.ExcludeDirs, cfg.IndexWorkers)

	cfg.LogLevel = LogLevel()
	return cfg
}

// applyEnvOverrides lets CODEGRAPH_* env vars override (or, for
// ExcludeDirs, append to) the YAML-loaded fields, then clamps every
// numeric field into its valid range. It is called unconditionally by
// LoadConfig, so YAML values get clamped even when no env var is set.
func applyEnvOverrides(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv(EnvExcludeDirs)); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.ExcludeDirs = append(cfg.ExcludeDirs, p)
			}
		}
	}
	if n, ok := envInt(EnvNice); ok {
		cfg.Nice = n
	}
	if n, ok := envInt(EnvMaxProcs); ok {
		cfg.MaxProcs = n
	}
	if n, ok := envInt(EnvCPUQuotaPercent); ok {
		cfg.CPUQuotaPercent = n
	}
	cfg.Nice = clampInt(cfg.Nice, 0, 19)
	cfg.MaxProcs = clampInt(cfg.MaxProcs, 0, 64)
	// CPUQuota maps to systemd CPUQuota=<N>%: 100 = one core, 6400 = a
	// 64-core host fully allotted. Higher values are meaningless — clamp.
	cfg.CPUQuotaPercent = clampInt(cfg.CPUQuotaPercent, 0, 6400)
}

// envInt parses an integer env var; ok=false when unset or not numeric.
func envInt(name string) (n int, ok bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("config: ignoring invalid %s=%q (not an integer)", name, v)
		return 0, false
	}
	return n, true
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// runtimeConfig is the concurrency-safe snapshot of the values the last
// LoadConfig produced that other packages read without taking a Config
// parameter (extraction skip rules, index worker pool sizing).
var runtimeConfig struct {
	sync.RWMutex
	excludeDirs  []string
	indexWorkers int
}

func storeRuntimeConfig(excludeDirs []string, indexWorkers int) {
	runtimeConfig.Lock()
	runtimeConfig.excludeDirs = append([]string(nil), excludeDirs...)
	runtimeConfig.indexWorkers = indexWorkers
	runtimeConfig.Unlock()
}

// ExcludeDirs returns the configured extra directory exclusion rules
// (YAML exclude_dirs plus CODEGRAPH_EXCLUDE_DIRS) captured by the most
// recent LoadConfig. The returned slice is a copy — safe to hold.
func ExcludeDirs() []string {
	runtimeConfig.RLock()
	defer runtimeConfig.RUnlock()
	return append([]string(nil), runtimeConfig.excludeDirs...)
}

// SetRuntimeConfigForTesting replaces the loaded-config snapshot consulted
// by ExcludeDirs and IndexWorkers. Tests should call it again with
// (nil, 0) to restore the defaults.
func SetRuntimeConfigForTesting(excludeDirs []string, indexWorkers int) {
	storeRuntimeConfig(excludeDirs, indexWorkers)
}

// expandPath expands a leading "~" (or a bare "~") to $HOME and expands
// environment variables ($VAR / ${VAR}) in config-supplied paths, so
// `workdirs: [~/proj]` and $PROJECT_ROOT resolve the way users expect.
// When $HOME is unresolvable the tilde is left as-is; unknown env vars
// expand to "".
func expandPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	return os.ExpandEnv(p)
}

// ConfigPath resolves the config file path in the standard lookup order:
// $CODEGRAPH_CONFIG, then ./codegraph-config.yaml, then
// ~/.config/codegraph/config.yaml. It returns "" when no config file exists.
// A $CODEGRAPH_CONFIG pointing at a missing file is skipped (L1) instead of
// being returned as-is, so callers never chase a dead path.
// An explicit -config flag is handled by LoadConfig; pass its value through
// when non-empty (see WorkdirAllowlist).
func ConfigPath() string {
	if p := os.Getenv("CODEGRAPH_CONFIG"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		// L1: used to be returned as-is, so every caller logged a read error
		// and fell back anyway — misleading when troubleshooting. Skip it and
		// continue the default lookup.
		log.Printf("config: $CODEGRAPH_CONFIG=%s does not exist; skipping and continuing the default lookup", p)
	}
	if _, err := os.Stat("./codegraph-config.yaml"); err == nil {
		return "./codegraph-config.yaml"
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".config", "codegraph", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// WorkdirAllowlist returns the authority roots for workdir validation. The
// config file is authoritative: when it exists and its "workdirs" key
// decodes to a non-empty list, those roots are returned (expanded for ~ and
// $VAR, then canonicalized by ValidateWorkdirs). Decoding uses the minimal
// workdirsOnlyConfig view, so a bad value under a non-privileged key
// (nice / max_procs / index_workers / cpu_quota_percent) can never widen the
// allowlist. Otherwise — no config file, unreadable file, a document whose
// YAML syntax itself is broken, or an empty workdirs list — the allowlist
// falls back to $HOME (canonicalized), so workdirs outside $HOME are refused
// even without a config file. When $HOME itself cannot be resolved the result
// is an empty allowlist, whose semantics are "reject everything" (fail closed
// — see ValidateWorkdirs).
// configFile may be empty to use the standard lookup order (ConfigPath).
func WorkdirAllowlist(configFile string) []string {
	path := configFile
	if path == "" {
		path = ConfigPath()
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			// Minimal decode on purpose (see workdirsOnlyConfig): an invalid
			// value under any other key must not degrade the authority roots
			// down to $HOME. A document whose YAML SYNTAX is broken still
			// fails to decode here — same contract as before: fall back to
			// $HOME (see TestWorkdirAllowlist's "workdirs: [unterminated"
			// case), never silently trust a partially parsed file.
			var wc workdirsOnlyConfig
			if err := yaml.Unmarshal(data, &wc); err == nil && len(wc.Workdirs) > 0 {
				// L2: expand ~ and $VAR exactly like LoadConfig does, so the
				// allowlist matches the expanded workdirs.
				roots := make([]string, 0, len(wc.Workdirs))
				for _, wd := range wc.Workdirs {
					if wd = expandPath(wd); wd != "" {
						roots = append(roots, wd)
					}
				}
				if len(roots) > 0 {
					return roots
				}
			}
		}
	}
	// No usable config: default the allowlist to $HOME.
	if home, err := os.UserHomeDir(); err == nil {
		if c := canonical(home); c != "" {
			return []string{c}
		}
	}
	return nil // fail closed: empty allowlist rejects every workdir
}

// canonical resolves p to an absolute path with symlinks evaluated, matching
// the canonicalization done in cmd/codegraph-go/main.go. When EvalSymlinks
// fails (e.g. the path does not exist) the absolute path is kept.
func canonical(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if rp, err := filepath.EvalSymlinks(abs); err == nil && rp != "" {
		return rp
	}
	return abs
}

// insideRoot reports whether p equals root or is a descendant of it, using
// path-segment containment: /root/codegraph-go is inside /root, but the
// sibling prefix /root-other is not.
func insideRoot(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	sep := string(filepath.Separator)
	return rel != ".." && !strings.HasPrefix(rel, ".."+sep)
}

// ValidateWorkdirs enforces the "config workdirs are the allowlist" policy:
// every workdir must equal one of the authority roots or be a descendant of
// one (path-segment containment). The allowlist comes from
// WorkdirAllowlist: the config file's workdirs, or $HOME as the default.
//
// Fail closed: an empty allowlist (no config file AND $HOME not resolvable)
// means "reject every workdir" — there is no loose mode. An allowlist that
// yields no usable canonical roots is treated the same way.
//
// Comparison is canonical: both sides are resolved with Abs + EvalSymlinks
// (same canonicalization as cmd/codegraph-go/main.go), so symlink escapes
// (e.g. /root/link -> /opt/...) are rejected.
//
// The returned error lists every offending workdir and the allowed roots, so
// callers can print an actionable message.
func ValidateWorkdirs(workdirs, allowlist []string) error {
	roots := make([]string, 0, len(allowlist))
	seen := make(map[string]bool)
	for _, r := range allowlist {
		c := canonical(r)
		if c != "" && !seen[c] {
			seen[c] = true
			roots = append(roots, c)
		}
	}
	if len(roots) == 0 {
		// Empty or unusable allowlist: reject everything (fail closed).
		return fmt.Errorf("empty workdir allowlist (no config file with workdirs and no resolvable $HOME); refusing to start (fail closed)")
	}
	var bad []string
	for _, wd := range workdirs {
		c := canonical(wd)
		if c == "" {
			bad = append(bad, wd)
			continue
		}
		ok := false
		for _, r := range roots {
			if insideRoot(c, r) {
				ok = true
				break
			}
		}
		if !ok {
			bad = append(bad, c)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("workdir(s) %q outside the codegraph authority roots %q; refusing to start", bad, roots)
}

// LogLevel returns the configured log level from the CODEGRAPH_LOG_LEVEL
// environment variable, defaulting to "info".
func LogLevel() string {
	if v := strings.TrimSpace(os.Getenv(EnvLogLevel)); v != "" {
		return v
	}
	return DefaultLogLevel
}

// IndexWorkers returns the number of parallel extraction workers.
// Priority: CODEGRAPH_INDEX_WORKERS env var (1 = serial rollback; cap 16)
// > index_workers loaded by the last LoadConfig > default
// min(8, NumCPU-1).
func IndexWorkers() int {
	if v := strings.TrimSpace(os.Getenv(EnvIndexWorkers)); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			if n < 1 {
				return 1
			}
			if n > 16 {
				return 16
			}
			return n
		}
	}
	runtimeConfig.RLock()
	cfgWorkers := runtimeConfig.indexWorkers
	runtimeConfig.RUnlock()
	if cfgWorkers > 0 {
		if cfgWorkers > 16 {
			return 16
		}
		return cfgWorkers
	}
	n := runtime.NumCPU() - 1
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// HomeIndexAll returns true when every top-level directory under $HOME should
// be indexed (not only project-like ones with go.mod / package.json / .git).
func HomeIndexAll() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvHomeIndexAll)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

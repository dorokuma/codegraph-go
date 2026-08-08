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

	"gopkg.in/yaml.v3"
)

// Env vars for extraction helpers (moved here so extraction packages don't
// read env vars directly).
const (
	EnvIndexWorkers = "CODEGRAPH_INDEX_WORKERS"
	EnvHomeIndexAll = "CODEGRAPH_GO_HOME_INDEX_ALL"
	EnvLogLevel     = "CODEGRAPH_LOG_LEVEL"
)

// Default log level when the env var is not set.
const DefaultLogLevel = "info"

// Config holds the top-level application configuration.
type Config struct {
	Workdir    string
	Workdirs   []string
	ConfigFile string
	NoSync     bool
	LogLevel   string
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
	flag.Parse()

	// Determine config file path: -config flag > ConfigPath() lookup order.
	configPath := cfg.ConfigFile
	if configPath == "" {
		configPath = ConfigPath()
	}

	// Read YAML config file.
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			log.Printf("config: read %s: %v", configPath, err)
		} else {
			var yamlCfg struct {
				Workdirs []string `yaml:"workdirs"`
			}
			if err := yaml.Unmarshal(data, &yamlCfg); err != nil {
				log.Printf("config: parse %s: %v (ignoring file)", configPath, err)
			} else if len(yamlCfg.Workdirs) > 0 {
				// L2: expand ~ and $VAR in config workdirs so `~/proj` and
				// $PROJECT_ROOT resolve the way users expect.
				cfg.Workdirs = make([]string, 0, len(yamlCfg.Workdirs))
				for _, wd := range yamlCfg.Workdirs {
					if wd = expandPath(wd); wd != "" {
						cfg.Workdirs = append(cfg.Workdirs, wd)
					}
				}
			}
		}
	}

	// -workdir flag overrides: prepend to the list if not already present.
	if cfg.Workdir != "" {
		cfg.Workdir = expandPath(cfg.Workdir)
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

	cfg.LogLevel = LogLevel()
	return cfg
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
// config file is authoritative: when it exists and parses to a non-empty
// workdirs list, those roots are returned (expanded for ~ and $VAR, then
// canonicalized by ValidateWorkdirs). Otherwise — no config file, unreadable
// or unparsable file, or an empty workdirs list — the allowlist falls back to
// $HOME (canonicalized), so workdirs outside $HOME are refused even without a
// config file. When $HOME itself cannot be resolved the result is an empty
// allowlist, whose semantics are "reject everything" (fail closed — see
// ValidateWorkdirs).
// configFile may be empty to use the standard lookup order (ConfigPath).
func WorkdirAllowlist(configFile string) []string {
	path := configFile
	if path == "" {
		path = ConfigPath()
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			var yamlCfg struct {
				Workdirs []string `yaml:"workdirs"`
			}
			if err := yaml.Unmarshal(data, &yamlCfg); err == nil && len(yamlCfg.Workdirs) > 0 {
				// L2: expand ~ and $VAR exactly like LoadConfig does, so the
				// allowlist matches the expanded workdirs.
				roots := make([]string, 0, len(yamlCfg.Workdirs))
				for _, wd := range yamlCfg.Workdirs {
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
// It reads CODEGRAPH_INDEX_WORKERS (1 = serial rollback; cap 16).
// When unset it returns runtime.NumCPU()-1, clamped to [1, 8].
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

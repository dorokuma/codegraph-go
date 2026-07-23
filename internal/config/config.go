// Package config provides centralised configuration for codegraph-go.
// It must not import any internal/business packages — only the standard library.
package config

import (
	"flag"
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
	EnvIndexWorkers  = "CODEGRAPH_INDEX_WORKERS"
	EnvHomeIndexAll  = "CODEGRAPH_GO_HOME_INDEX_ALL"
	EnvLogLevel      = "CODEGRAPH_LOG_LEVEL"
)

// Default log level when the env var is not set.
const DefaultLogLevel = "info"

// Config holds the top-level application configuration.
type Config struct {
	Workdir     string
	Workdirs    []string
	ConfigFile  string
	NoSync      bool
	LogLevel    string
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

	// Determine config file path.
	configPath := cfg.ConfigFile
	if configPath == "" {
		configPath = os.Getenv("CODEGRAPH_CONFIG")
	}
	if configPath == "" {
		if _, err := os.Stat("./codegraph-config.yaml"); err == nil {
			configPath = "./codegraph-config.yaml"
		}
	}
	if configPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".config", "codegraph", "config.yaml")
			if _, err := os.Stat(p); err == nil {
				configPath = p
			}
		}
	}

	// Read YAML config file.
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			var yamlCfg struct {
				Workdirs []string `yaml:"workdirs"`
			}
			if err := yaml.Unmarshal(data, &yamlCfg); err == nil && len(yamlCfg.Workdirs) > 0 {
				cfg.Workdirs = yamlCfg.Workdirs
			}
		}
	}

	// -workdir flag overrides: prepend to the list if not already present.
	if cfg.Workdir != "" {
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

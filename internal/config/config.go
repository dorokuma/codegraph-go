// Package config provides centralised configuration for codegraph-go.
// It must not import any internal/business packages — only the standard library.
package config

import (
	"flag"
	"os"
	"runtime"
	"strconv"
	"strings"
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
	Workdir string
	NoSync  bool
	LogLevel string
}

// LoadConfig parses CLI flags, reads the environment, and returns a Config
// with defaults applied.  It is safe to call multiple times (flag re‑parse
// panics, so this should be called exactly once).
func LoadConfig() Config {
	var cfg Config

	flag.StringVar(&cfg.Workdir, "workdir", "", "workspace root (default: cwd)")
	flag.BoolVar(&cfg.NoSync, "no-sync", false, "disable auto-sync file watcher")
	flag.Parse()

	if cfg.Workdir == "" {
		wd, err := os.Getwd()
		if err == nil {
			cfg.Workdir = wd
		}
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

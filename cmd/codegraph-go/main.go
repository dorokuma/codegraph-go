// codegraph-go: a Go MCP server with SQLite indexing, auto-sync, and code intelligence.
//
// MCP tools (official 8 + affected): explore, node, search, callers, callees, impact, files, status, affected.
package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dorokuma/codegraph-go/internal/config"
	"github.com/dorokuma/codegraph-go/internal/daemon"
	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/safelog"
	"github.com/dorokuma/codegraph-go/internal/server"
)

// runInit implements `codegraph-go init <root>` for hosts that pre-warm the
// index directory (e.g. reasonix). It only ensures the DB layout exists and
// returns quickly — full indexing happens when the MCP server starts.
func runInit(root string) error {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	database, err := db.Open(abs)
	if err != nil {
		return err
	}
	if err := database.Close(); err != nil {
		return err
	}
	slog.Info("init ok", "workdir", abs, "db", filepath.Join(abs, ".codegraph", "codegraph.db"))
	return nil
}

// checkDirectFallbackSafe verifies the index is not owned by another live
// process before falling back to direct mode (B1). If a daemon still holds
// the DB — e.g. it survived the version-mismatch kill — running direct would
// double-write the same index, so we return an actionable error instead.
func checkDirectFallbackSafe(root string) error {
	pidPath := daemon.PidPath(root)
	if raw, err := os.ReadFile(pidPath); err == nil {
		if info := daemon.DecodeLock(raw); info != nil && info.PID > 0 && daemon.IsProcessAlive(info.PID) {
			return fmt.Errorf("database in use by another process (daemon pid %d): stop the daemon first, or set CODEGRAPH_NO_DAEMON=1 to disable the shared daemon", info.PID)
		}
	}
	// DB-level probe: a non-daemon writer may hold the index too.
	database, err := db.Open(root)
	if err != nil {
		if strings.Contains(err.Error(), "in use") || strings.Contains(err.Error(), "locked") {
			return fmt.Errorf("database in use by another process: %v (stop the other process first, or set CODEGRAPH_NO_DAEMON=1)", err)
		}
		return err
	}
	_ = database.Close()
	return nil
}

func main() {
	_, safelogCleanup := safelog.SetupLogger(config.LogLevel())
	defer safelogCleanup()

	// Subcommand: init <root>  (must be handled before flag.Parse)
	if len(os.Args) >= 2 && os.Args[1] == "init" {
		root := "."
		if len(os.Args) >= 3 {
			root = os.Args[2]
		}
		if err := runInit(root); err != nil {
			log.Fatalf("init: %v", err)
		}
		return
	}

	cfg := config.LoadConfig()

	// Canonicalize all workdirs (abs + EvalSymlinks) and dedup.
	seen := make(map[string]bool)
	var unique []string
	for _, wd := range cfg.Workdirs {
		absWd, err := filepath.Abs(wd)
		if err != nil {
			log.Fatalf("bad workdir %q: %v", wd, err)
		}
		if rp, err := filepath.EvalSymlinks(absWd); err == nil && rp != "" {
			absWd = rp
		}
		if !seen[absWd] {
			seen[absWd] = true
			unique = append(unique, absWd)
		}
	}
	cfg.Workdirs = unique
	// Primary workdir for backward compat (cfg.Workdir = workdirs[0]).
	cfg.Workdir = cfg.Workdirs[0]
	slog.Info("starting", "workdir", cfg.Workdir, "workdirs", cfg.Workdirs)

	// Decision order (official #411):
	//  1. CODEGRAPH_DAEMON_INTERNAL=1 → we ARE the detached daemon
	//  2. CODEGRAPH_NO_DAEMON=1 → direct embedded mode
	//  3. else try proxy to shared daemon (spawn if needed); fallback direct
	if daemon.Internal() {
		if err := server.RunDaemonProcess(cfg); err != nil {
			slog.Error("daemon process failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if daemon.OptOut() {
		slog.Info("mode=direct (CODEGRAPH_NO_DAEMON)")
		if err := server.RunDirect(cfg); err != nil {
			slog.Error("runDirect failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Need a place for lock/socket. Prefer first existing .codegraph/ across
	// all workdirs; otherwise use primary workdir.
	root := ""
	for _, wd := range cfg.Workdirs {
		root = db.FindNearestCodeGraphRoot(wd)
		if root != "" {
			break
		}
	}
	if root == "" {
		root = cfg.Workdirs[0]
		// Ensure .codegraph exists so a daemon can be spawned.
		if database, err := db.Open(root); err == nil {
			_ = database.Close()
		}
	}
	if rp, err := filepath.EvalSymlinks(root); err == nil && rp != "" {
		root = rp
	}

	// Probe → spawn → dial shared daemon; on failure fall back to direct.
	// Pass parent -config / -no-sync so the detached writer matches this session.
	spawnOpts := &daemon.SpawnOpts{ConfigFile: cfg.ConfigFile, NoSync: cfg.NoSync}
	conn, br, hello, ok := daemon.EnsureAndDial(root, 6*time.Second, 25*time.Millisecond, spawnOpts)
	if ok {
		slog.Info("proxy → daemon", "pid", hello.PID, "socket", hello.SocketPath)
		_ = daemon.RunProxy(conn, br, hello)
		return
	}
	slog.Info("mode=direct (daemon unavailable)")
	// Never double-write: refuse direct mode while a live daemon still owns
	// the index, with an actionable error and a non-zero exit.
	if err := checkDirectFallbackSafe(root); err != nil {
		fmt.Fprintf(os.Stderr, "codegraph-go: %v\n", err)
		slog.Error("direct fallback blocked", "error", err)
		os.Exit(1)
	}
	if err := server.RunDirect(cfg); err != nil {
		slog.Error("runDirect failed", "error", err)
		os.Exit(1)
	}
}

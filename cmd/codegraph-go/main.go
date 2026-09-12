// codegraph-go: a Go MCP server with SQLite indexing, auto-sync, and code intelligence.
//
// MCP tools (official 8 + affected): explore, node, search, callers, callees, impact, files, status, affected.
package main

import (
	"errors"
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
	// Authority check (same policy as main, no side door): init <root> must
	// stay inside the allowlist roots — config workdirs, or $HOME when there
	// is no usable config (fail closed when neither resolves).
	if err := config.ValidateWorkdirs([]string{root}, config.WorkdirAllowlist("")); err != nil {
		return fmt.Errorf("%v. Fix: use a root inside an allowed root, or add it to the workdirs list in the config file", err)
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

// dbInUseMessage builds the canonical actionable "database in use" wording
// shared by every entry path. suggestNoDaemon controls whether the message
// suggests setting CODEGRAPH_NO_DAEMON=1: in NO_DAEMON direct mode that hint
// is already in effect, so appending it would be self-contradictory.
func dbInUseMessage(detail string, suggestNoDaemon bool) string {
	if suggestNoDaemon {
		return fmt.Sprintf("database in use by another process: %s (stop the other process first, or set CODEGRAPH_NO_DAEMON=1)", detail)
	}
	return fmt.Sprintf("database in use by another process: %s (stop the other process first)", detail)
}

// dbInUseError rewrites a db.Open failure into the canonical actionable
// "database in use" message when the underlying cause is a held lock, so
// every entry path (daemon-fallback probe, CODEGRAPH_NO_DAEMON direct mode)
// reports one consistent wording and exit code instead of a raw Open error.
// suggestNoDaemon is forwarded to dbInUseMessage (see G2).
func dbInUseError(err error, suggestNoDaemon bool) error {
	if err == nil {
		return nil
	}
	// Preferred detection: the db layer wraps its own lock conflict in
	// db.ErrIndexInUse, so this does not depend on message wording. The
	// string fallbacks stay for errors that cannot be pre-wrapped by the
	// db layer: driver-level SQLITE_BUSY ("database is locked") and older
	// binary "in use" wording still in flight.
	if errors.Is(err, db.ErrIndexInUse) ||
		strings.Contains(err.Error(), "in use") ||
		strings.Contains(err.Error(), "locked") {
		return errors.New(dbInUseMessage(err.Error(), suggestNoDaemon))
	}
	return err
}

// checkDirectFallbackSafe verifies the index is not owned by another live
// process before falling back to direct mode (B1). If a daemon still holds
// the DB — e.g. it survived the version-mismatch kill — running direct would
// double-write the same index, so we return an actionable error instead.
func checkDirectFallbackSafe(root string) error {
	pidPath := daemon.PidPath(root)
	if raw, err := os.ReadFile(pidPath); err == nil {
		if info := daemon.DecodeLock(raw); info != nil && info.PID > 0 && daemon.IsProcessAlive(info.PID) {
			return errors.New(dbInUseMessage(fmt.Sprintf("daemon pid %d", info.PID), true))
		}
	}
	// DB-level probe: a non-daemon writer may hold the index too.
	database, err := db.Open(root)
	if err != nil {
		return dbInUseError(err, true)
	}
	_ = database.Close()
	return nil
}

// fatalFlushTimeout bounds how long the exit paths below wait for the
// non-blocking log buffer to drain before the process exits. Generous
// default: stderr is normally a file or a read pipe, so the drain is
// instant; the bound only matters for a stuck stderr, where dropping beats
// hanging forever.
const fatalFlushTimeout = 2 * time.Second

// fatalf reports a fatal error with the exact log.Fatalf output format and
// exit code 1, but flushes the non-blocking log buffer first. log.Fatalf
// cannot be used on these paths: its internal os.Exit skips deferred cleanup
// and races the asynchronous drain goroutine, so the fatal line — and
// anything else still buffered — was silently dropped and every such failure
// reached the user as an empty stderr (red team: `codegraph init` on a
// symlinked .codegraph exited rc=1 with 0 bytes on stderr, 20/20 runs).
func fatalf(format string, args ...any) {
	log.Printf(format, args...)
	safelog.Flush(fatalFlushTimeout)
	os.Exit(1)
}

// flushAndExit exits with code after flushing buffered log lines. The
// failure paths below log through slog, whose writes go through the
// non-blocking writer, so a bare os.Exit drops the structured error line
// (the fmt.Fprintln(os.Stderr, …) lines on the same paths are synchronous
// and unaffected; the flush is for the slog line that follows them).
func flushAndExit(code int) {
	safelog.Flush(fatalFlushTimeout)
	os.Exit(code)
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
			fatalf("init: %v", err)
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
			fatalf("bad workdir %q: %v", wd, err)
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
	// M7: LoadConfig leaves Workdirs empty only when os.Getwd fails AND there
	// is no -workdir/config source; indexing [0] below would panic. Fail with
	// an actionable message instead.
	if len(cfg.Workdirs) == 0 {
		fatalf("no workdir: current directory cannot be resolved (os.Getwd failed) and no workdirs from -workdir or config; run from an existing directory or pass -workdir")
	}
	// Primary workdir for backward compat (cfg.Workdir = workdirs[0]).
	cfg.Workdir = cfg.Workdirs[0]

	// Authority check: the config file's workdirs are the allowlist — any
	// workdir outside those roots (or not a descendant of one) is refused
	// before any mode is entered. This single call point covers client,
	// daemon (CODEGRAPH_DAEMON_INTERNAL) and direct (CODEGRAPH_NO_DAEMON)
	// paths alike, since all of them run through main. WorkdirAllowlist
	// semantics: the config file's workdirs when a usable config exists,
	// otherwise $HOME as the fallback root — validation ALWAYS runs (it is
	// never skipped for "no config"; when $HOME itself is unresolvable the
	// empty allowlist fails closed and every workdir is refused).
	configFile := cfg.ConfigFile
	if configFile == "" {
		configFile = config.ConfigPath()
	}
	if err := config.ValidateWorkdirs(cfg.Workdirs, config.WorkdirAllowlist(configFile)); err != nil {
		msg := fmt.Sprintf("codegraph-go: %v. Fix: use a workdir inside an allowed root, or add it to the workdirs list in the config file (%s)", err, configFile)
		fmt.Fprintln(os.Stderr, msg)
		slog.Error("workdir outside authority roots", "error", err, "config", configFile)
		flushAndExit(1)
	}
	slog.Info("starting", "workdir", cfg.Workdir, "workdirs", cfg.Workdirs)

	// Decision order (official #411):
	//  1. CODEGRAPH_DAEMON_INTERNAL=1 → we ARE the detached daemon
	//  2. CODEGRAPH_NO_DAEMON=1 → direct embedded mode
	//  3. else try proxy to shared daemon (spawn if needed); fallback direct
	if daemon.Internal() {
		if err := server.RunDaemonProcess(cfg); err != nil {
			slog.Error("daemon process failed", "error", err)
			flushAndExit(1)
		}
		return
	}
	if daemon.OptOut() {
		slog.Info("mode=direct (CODEGRAPH_NO_DAEMON)")
		// S4: fail fast with the canonical "database in use" error and exit
		// code (same wording as checkDirectFallbackSafe) when another process
		// holds the index, instead of a raw Open error from inside RunDirect.
		// G2: CODEGRAPH_NO_DAEMON is already set here, so the message must not
		// suggest setting it again.
		if database, err := db.Open(cfg.Workdir); err != nil {
			err = dbInUseError(err, false)
			fmt.Fprintf(os.Stderr, "codegraph-go: %v\n", err)
			slog.Error("direct mode blocked", "error", err)
			flushAndExit(1)
		} else {
			_ = database.Close()
		}
		if err := server.RunDirect(cfg); err != nil {
			slog.Error("runDirect failed", "error", err)
			flushAndExit(1)
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
	conn, br, hello, ok, err := daemon.EnsureAndDial(root, 6*time.Second, 25*time.Millisecond, spawnOpts)
	if ok {
		slog.Info("proxy → daemon", "pid", hello.PID, "socket", hello.SocketPath)
		if _, err := daemon.RunProxy(conn, br, hello); err != nil {
			// L2: the hello write failed — the socket is unusable. Log it and
			// exit non-zero (like the ErrStaleDaemonRefused path): direct
			// fallback would double-write (the daemon is alive and still owns
			// the DB lock) and the host must know the proxy was never built.
			slog.Error("proxy failed", "error", err)
			flushAndExit(1)
		}
		return
	}
	if err != nil && errors.Is(err, daemon.ErrStaleDaemonRefused) {
		// G1: the PID-reuse guard refused to kill the stale daemon because an
		// unidentified live process holds the lock. Neither spawning on top of
		// it nor falling back to direct mode is safe (double-write risk), so
		// surface an actionable error and exit non-zero instead.
		fmt.Fprintf(os.Stderr, "codegraph-go: %v; remove the pidfile manually and retry\n", err)
		slog.Error("daemon path blocked: unidentified process holds the daemon lock", "error", err)
		flushAndExit(1)
	}
	if err != nil && errors.Is(err, daemon.ErrInvisibleHolderSurvived) {
		// G3: an invisible flock holder survived SIGTERM+SIGKILL — a live
		// process still owns the DB flock and cannot be killed. Neither
		// spawning a replacement nor falling back to direct mode is safe
		// (double-write risk), so surface an actionable message (root + pgrep
		// hint for manual cleanup) and exit non-zero, parallel to the
		// ErrStaleDaemonRefused path.
		fmt.Fprintf(os.Stderr, "codegraph-go: %v; the lock for %s is held by a process that survived SIGTERM+SIGKILL — identify it with: pgrep -af codegraph-go (look for '-workdir %s'), kill it manually, then retry\n", err, root, root)
		slog.Error("daemon path blocked: invisible flock holder survived SIGTERM+SIGKILL", "error", err, "root", root)
		flushAndExit(1)
	}
	slog.Info("mode=direct (daemon unavailable)")
	// Never double-write: refuse direct mode while a live daemon still owns
	// the index, with an actionable error and a non-zero exit.
	if err := checkDirectFallbackSafe(root); err != nil {
		fmt.Fprintf(os.Stderr, "codegraph-go: %v\n", err)
		slog.Error("direct fallback blocked", "error", err)
		flushAndExit(1)
	}
	if err := server.RunDirect(cfg); err != nil {
		slog.Error("runDirect failed", "error", err)
		flushAndExit(1)
	}
}

package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/dorokuma/codegraph-go/internal/cgdir"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type DB struct {
	// RWMutex: readers (search/callers) don't block each other; writers still exclusive.
	mu   sync.RWMutex
	conn *sql.DB
	path string
	// lockFile is the process-level exclusive lock on .codegraph/codegraph.lock
	// (A1 single-writer). Held for the lifetime of the DB; released on Close.
	lockFile *os.File
	// pinned is the dirfd pinning the validated .codegraph directory. On
	// Linux the connection DSN is built as /proc/self/fd/<fd>/codegraph.db
	// (see pinnedDBPath), so the fd must stay open for the whole connection
	// lifetime — it is released in Close, after the last connection is gone.
	pinned *pinnedDir
}

// ErrIndexInUse marks the A1 single-writer lock conflict: Open failed
// because another process holds .codegraph/codegraph.lock. Callers match it
// with errors.Is instead of string-matching the message. Its text is the
// static prefix of the historical in-use error so the rendered message is
// unchanged; the holder hint and the underlying flock error follow it.
var ErrIndexInUse = errors.New("codegraph.db in use by another process")

// Open opens (or creates) the SQLite database at .codegraph/codegraph.db under workdir.
func Open(workdir string) (db *DB, err error) {
	// Resolve to an absolute path up front. The DSN is a file:// URI where a
	// relative path would be misparsed — "file://proj/.codegraph/codegraph.db"
	// treats "proj" as the URI host. Absolute also keeps Path(), WALSize()
	// etc. stable regardless of the caller's cwd.
	absWorkdir, aerr := filepath.Abs(workdir)
	if aerr != nil {
		return nil, fmt.Errorf("resolve workdir %q: %w", workdir, aerr)
	}
	workdir = absWorkdir

	dir := filepath.Join(workdir, ".codegraph")
	// Symlink jail (adversarial audit): a symlinked .codegraph makes the
	// MkdirAll inside a no-op, and SQLite would then create codegraph.db,
	// -wal, -shm and codegraph.lock at the symlink TARGET - outside the
	// project. cgdir.Ensure fails closed in two independent steps (Lstat,
	// realpath) before a single file is written; the dirfd pinning below
	// closes the remaining post-validation TOCTOU.
	if _, err := cgdir.Ensure(dir); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "codegraph.db")

	// TOCTOU hardening: the validation above and every path resolution below
	// it are independent — the red team won by polling for codegraph.lock and
	// swapping .codegraph for a symlink inside the gap, sending codegraph.db
	// to the symlink target. Pin the validated directory with a dirfd and
	// create the lock file through it: whatever the path does afterwards, the
	// pin holds the very inode that passed the checks, and the dev/ino
	// rechecks below detect any swap that happened in between.
	pinned, perr := pinDir(dir)
	if perr != nil {
		return nil, perr
	}
	// The pin lives as long as the DB: on Linux the connection DSN is a
	// /proc/self/fd/<fd> magic link, so closing the fd would sever the DSN
	// under every pooled connection. Release it here only when Open fails;
	// on success the DB owns it and Close frees it after the connections.
	defer func() {
		if err != nil || db == nil {
			_ = pinned.close()
		}
	}()

	// A1: single-writer lock. SQLite WAL allows one writer; a second process
	// opening the same index would fight over the write lock (busy errors,
	// lost updates). Take a process-level exclusive flock on codegraph.lock
	// before touching the db; fail fast with a clear error when held. The
	// file is created through the pinned dirfd (O_NOFOLLOW), so creation
	// cannot escape the validated directory.
	lockFile, lerr := pinned.openLockFile(dir, "codegraph.lock")
	if lerr != nil {
		return nil, fmt.Errorf("open lock file: %w", lerr)
	}
	if ferr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("%w%s: %w", ErrIndexInUse, lockHolderHint(dir), ferr)
	}
	// Every error path below must release the lock before returning.
	defer func() {
		if err != nil {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
		}
	}()

	// The flock is ours through the pinned directory: verify the .codegraph
	// path still resolves to the pinned inode. Anything else means the
	// directory was swapped between validation and locking — abort before a
	// single db byte is written. (The deferred cleanup above releases the
	// lock and closes both fds on every error path below.)
	if err := pinned.recheckDir(dir); err != nil {
		return nil, err
	}

	// DSN pragmas ensure every connection gets foreign_keys + busy_timeout,
	// not just the first one in the pool (database/sql may open new connections
	// concurrently, and default is foreign_keys=OFF / busy_timeout=0).
	// On Linux the path part is /proc/self/fd/<pinned fd>/codegraph.db (see
	// pinnedDBPath): SQLite's own file resolutions — db, -wal, -shm — then
	// always land in the pinned directory, whatever the .codegraph path does
	// after validation. An unusable procfs magic link fails closed there
	// unless CODEGRAPH_ALLOW_PLAIN_DB=1 opts into the degraded plain path
	// (see pinnedDBPath); on other platforms this is the validated plain
	// path. Escape URI-special characters in the path so spaces / # / ? / &
	// work.
	dsnPath, dsnErr := pinnedDBPath(pinned, dir)
	if dsnErr != nil {
		return nil, dsnErr
	}
	dsn := sqliteFileDSN(dsnPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// W12: a single pooled connection. WAL writes serialize through one handle
	// and the DB RWMutex never races against a second connection; combined with
	// the process lock above this makes writes strictly single-writer.
	conn.SetMaxOpenConns(1)

	// Enable WAL mode for concurrent reads
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	// Post-creation recheck (defense in depth): on non-Linux SQLite receives
	// the plain dbPath and its file creations re-resolve the path
	// independently of the pin, so this comparison is the only escape
	// detection there. On Linux the DSN goes through the pinned fd's magic
	// link and creations cannot escape; the recheck stays as a cheap,
	// redundant verification that the on-disk state matches the pin. Either
	// way it is detection, not cleanup: if this fires, stray files may
	// already exist outside and are deliberately NOT unlinked (deleting
	// through a freshly swapped path would touch attacker-chosen names).
	if err := pinned.recheckChild(dir, "codegraph.db", dbPath); err != nil {
		conn.Close()
		return nil, err
	}

	// Set busy timeout
	if _, err := conn.Exec("PRAGMA busy_timeout=5000"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	// Enforce FK so unresolved_refs / edges cascade when nodes are deleted.
	if _, err := conn.Exec("PRAGMA foreign_keys=ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable foreign_keys: %w", err)
	}

	db = &DB{conn: conn, path: dbPath, lockFile: lockFile, pinned: pinned}

	// Finish any edges rebuild interrupted by a crash in pre-transaction
	// builds BEFORE schema.sql runs: schema.sql would otherwise recreate an
	// empty edges table and orphan the rows still sitting in edges_new.
	if err := db.recoverEdgesRebuild(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("recover edges rebuild: %w", err)
	}

	// Apply schema (CREATE IF NOT EXISTS — does not ALTER existing tables).
	if _, err := conn.Exec(schemaSQL); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Older DBs may predate meta; ensure it exists even if schema embed was cached.
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ensure meta: %w", err)
	}

	// Bring pre-v7 tables up to current columns/indexes without wiping data here.
	// Logic-version mismatch still triggers Wipe+Rebuild separately.
	if err := db.ensureSchema(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ensure schema: %w", err)
	}
	// Older DBs may have nodes_fts without the tokenize clause; rebuild first
	// so ensureFTSBackfill only rebuilds once (not once with old tokenize then
	// again after the DROP+recreate below).
	if err := db.ensureFTSTokenize(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("fts tokenize: %w", err)
	}
	// Old indexes created before FTS need a one-time backfill; triggers only
	// cover rows written after the FTS table exists.
	if err := db.ensureFTSBackfill(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("fts backfill: %w", err)
	}

	return db, nil
}

// ensureFTSBackfill rebuilds nodes_fts when it is out of sync with nodes
// (typical after upgrading a pre-FTS database).
//
// NOTE: COUNT(*) on an external-content FTS5 table tracks the content table,
// not the index. Use the shadow docsize table to detect an empty/stale index.
func (d *DB) ensureFTSBackfill() error {
	var nodeCount, docCount int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount); err != nil {
		return fmt.Errorf("count nodes: %w", err)
	}
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM nodes_fts_docsize`).Scan(&docCount); err != nil {
		return fmt.Errorf("count nodes_fts_docsize: %w", err)
	}
	if nodeCount == docCount {
		return nil
	}
	// FTS5 external-content rebuild from the nodes table.
	if _, err := d.conn.Exec(`INSERT INTO nodes_fts(nodes_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild nodes_fts: %w", err)
	}
	return nil
}

// Close closes the database connection, releases the single-writer lock and
// the directory pin. Idempotent: a second Close is a no-op returning nil
// (the connection, the flock and the pin are each released exactly once).
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var err error
	if d.conn != nil {
		err = d.conn.Close()
		d.conn = nil
	}
	if d.lockFile != nil {
		// LOCK_UN + close releases the A1 process lock so the next Open wins.
		_ = syscall.Flock(int(d.lockFile.Fd()), syscall.LOCK_UN)
		_ = d.lockFile.Close()
		d.lockFile = nil
	}
	if d.pinned != nil {
		// Released after the connection: on Linux the DSN is a
		// /proc/self/fd/<fd> magic link that must stay resolvable until the
		// last SQLite handle (including its -wal/-shm cleanup on close) is
		// done with it.
		_ = d.pinned.close()
		d.pinned = nil
	}
	return err
}

// lockHolderHint enriches the in-use error with daemon pid/version when a
// .codegraph/daemon.pid file is present (best-effort; returns "" otherwise).
// Parsed inline (not via the daemon package) to avoid an import cycle.
func lockHolderHint(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "daemon.pid"))
	if err != nil {
		return ""
	}
	var info struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.PID <= 0 {
		return ""
	}
	// Self-reference guard: a daemon writes its OWN pidfile before opening
	// the DB. When the open then fails on a flock held by an INVISIBLE
	// holder (pidfile/socket dentry removed by a deploy race while that
	// daemon kept running), the recorded pid is this very process —
	// reporting "held by pid <self>" would be a lie about who owns the
	// lock and actively mislead debugging. Return no hint; the error
	// already says "another process" and stays accurate.
	if info.PID == os.Getpid() {
		return ""
	}
	hint := fmt.Sprintf(" (held by pid %d", info.PID)
	if info.Version != "" {
		hint += ", version " + info.Version
	}
	return hint + ")"
}

// Path returns the database file path.
func (d *DB) Path() string {
	return d.path
}

// sqliteFileDSN builds a modernc.org/sqlite URI with path characters escaped
// so spaces, #, ?, and & in the filesystem path are not parsed as URI syntax.
func sqliteFileDSN(dbPath string) string {
	// Percent-encode URI-significant characters only; keep path separators.
	esc := strings.NewReplacer(
		"%", "%25",
		"?", "%3F",
		"#", "%23",
		"&", "%26",
		" ", "%20",
	).Replace(dbPath)
	// file:///abs/path form: "file://" + "/abs/..." → three slashes.
	return "file://" + esc + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
}

// ensureFTSTokenize checks whether nodes_fts has the tokenize clause and
// rebuilds it if missing (pre-S-19 schema). CREATE VIRTUAL TABLE IF NOT EXISTS
// does not alter an existing FTS table, so we must detect and rebuild manually.
func (d *DB) ensureFTSTokenize() error {
	var ddl string
	if err := d.conn.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='nodes_fts'`,
	).Scan(&ddl); err != nil {
		// Table doesn't exist yet — schema SQL will create it with tokenize.
		return nil
	}
	if strings.Contains(ddl, "tokenchars") {
		return nil
	}
	// Rebuild: drop old FTS table, re-apply schema, then backfill.
	if _, err := d.conn.Exec(`DROP TABLE IF EXISTS nodes_fts`); err != nil {
		return fmt.Errorf("drop old nodes_fts: %w", err)
	}
	// Recreate with tokenize via schema SQL (the FTS table portion).
	if _, err := d.conn.Exec(schemaSQL); err != nil {
		return fmt.Errorf("recreate nodes_fts: %w", err)
	}
	if _, err := d.conn.Exec(`INSERT INTO nodes_fts(nodes_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild nodes_fts after tokenize fix: %w", err)
	}
	return nil
}

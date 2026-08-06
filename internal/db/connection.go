package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

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
}

// Open opens (or creates) the SQLite database at .codegraph/codegraph.db under workdir.
func Open(workdir string) (db *DB, err error) {
	dir := filepath.Join(workdir, ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create .codegraph dir: %w", err)
	}
	dbPath := filepath.Join(dir, "codegraph.db")

	// A1: single-writer lock. SQLite WAL allows one writer; a second process
	// opening the same index would fight over the write lock (busy errors,
	// lost updates). Take a process-level exclusive flock on codegraph.lock
	// before touching the db; fail fast with a clear error when held.
	lockPath := filepath.Join(dir, "codegraph.lock")
	lockFile, lerr := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if lerr != nil {
		return nil, fmt.Errorf("open lock file: %w", lerr)
	}
	if ferr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("codegraph.db in use by another process%s: %w", lockHolderHint(dir), ferr)
	}
	// Every error path below must release the lock before returning.
	defer func() {
		if err != nil {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
		}
	}()

	// DSN pragmas ensure every connection gets foreign_keys + busy_timeout,
	// not just the first one in the pool (database/sql may open new connections
	// concurrently, and default is foreign_keys=OFF / busy_timeout=0).
	// Escape URI-special characters in the path so spaces / # / ? / & work.
	dsn := sqliteFileDSN(dbPath)
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

	db = &DB{conn: conn, path: dbPath, lockFile: lockFile}
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

// Close closes the database connection and releases the single-writer lock.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var err error
	if d.conn != nil {
		err = d.conn.Close()
	}
	if d.lockFile != nil {
		// LOCK_UN + close releases the A1 process lock so the next Open wins.
		_ = syscall.Flock(int(d.lockFile.Fd()), syscall.LOCK_UN)
		_ = d.lockFile.Close()
		d.lockFile = nil
	}
	return err
}

// lockHolderHint enriches the in-use error with daemon pid/version when a
// .codegraph/daemon.pid file is present (best-effort;

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

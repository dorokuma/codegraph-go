package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type DB struct {
	// RWMutex: readers (search/callers) don't block each other; writers still exclusive.
	mu   sync.RWMutex
	conn *sql.DB
	path string
}

// Open opens (or creates) the SQLite database at .codegraph/codegraph.db under workdir.
func Open(workdir string) (*DB, error) {
	dir := filepath.Join(workdir, ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create .codegraph dir: %w", err)
	}
	dbPath := filepath.Join(dir, "codegraph.db")

	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

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

	db := &DB{conn: conn, path: dbPath}
	// Bring pre-v7 tables up to current columns/indexes without wiping data here.
	// Logic-version mismatch still triggers Wipe+Rebuild separately.
	if err := db.ensureSchema(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ensure schema: %w", err)
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

// Close closes the database connection.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}

// Path returns the database file path.
func (d *DB) Path() string {
	return d.path
}

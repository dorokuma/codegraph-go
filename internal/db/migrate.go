package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// ensureSchema upgrades an existing on-disk DB that was created before
// schema.sql gained new columns/tables. CREATE TABLE IF NOT EXISTS does not
// alter existing tables, so we ADD COLUMN when missing.
//
// Safe to call on a fresh DB (all columns already present → no-ops).
func (d *DB) ensureSchema() error {
	if err := d.addMissingColumns("nodes", []colDef{
		{"qualified_name", "TEXT"},
		{"signature", "TEXT"},
		{"docstring", "TEXT"},
		{"start_column", "INTEGER"},
		{"end_column", "INTEGER"},
		{"visibility", "TEXT"},
		{"is_exported", "INTEGER DEFAULT 0"},
		{"return_type", "TEXT"},
	}); err != nil {
		return err
	}
	if err := d.addMissingColumns("edges", []colDef{
		{"col", "INTEGER"},
		{"provenance", "TEXT"},
		{"metadata", "TEXT"},
	}); err != nil {
		return err
	}
	// A3: pre-A3 databases have UNIQUE(source_id,target_id,kind) which collapses
	// multi call-site edges; rebuild the table with the (kind,line,col) key.
	if err := d.rebuildEdgesUniqueKey(); err != nil {
		return err
	}
	if err := d.addMissingColumns("files", []colDef{
		{"content_hash", "TEXT"},
		{"language", "TEXT"},
		{"node_count", "INTEGER DEFAULT 0"},
	}); err != nil {
		return err
	}
	// unresolved_refs is created by schema.sql; re-assert indexes for older
	// embeds that only had the CREATE TABLE without later indexes.
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS idx_nodes_qualified_name ON nodes(qualified_name)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_provenance ON edges(provenance)`,
		`CREATE INDEX IF NOT EXISTS idx_files_language ON files(language)`,
		`CREATE INDEX IF NOT EXISTS idx_unresolved_from_node ON unresolved_refs(from_node)`,
		`CREATE INDEX IF NOT EXISTS idx_unresolved_name ON unresolved_refs(reference_name)`,
		`CREATE INDEX IF NOT EXISTS idx_unresolved_file_path ON unresolved_refs(file_path)`,
		`CREATE INDEX IF NOT EXISTS idx_unresolved_status ON unresolved_refs(status)`,
		`CREATE INDEX IF NOT EXISTS idx_unresolved_failed_tail ON unresolved_refs(name_tail) WHERE status = 'failed'`,
	} {
		if _, err := d.conn.Exec(q); err != nil {
			// unresolved_refs may not exist yet if schema embed failed earlier;
			// surface real errors only when the table is present.
			if strings.Contains(err.Error(), "no such table: unresolved_refs") {
				continue
			}
			return fmt.Errorf("ensure index: %s: %w", q, err)
		}
	}
	return nil
}

type colDef struct {
	name string
	decl string // full type + optional DEFAULT, e.g. "TEXT" or "INTEGER DEFAULT 0"
}

func (d *DB) addMissingColumns(table string, cols []colDef) error {
	existing, err := d.tableColumns(table)
	if err != nil {
		return err
	}
	for _, c := range cols {
		if existing[c.name] {
			continue
		}
		q := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, c.name, c.decl)
		if _, err := d.conn.Exec(q); err != nil {
			// Concurrent open / already-added race: ignore duplicate column.
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("add column %s.%s: %w", table, c.name, err)
		}
	}
	return nil
}

func (d *DB) tableColumns(table string) (map[string]bool, error) {
	rows, err := d.conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// tableSQL returns the stored CREATE TABLE DDL for a table ("" when missing).
func (d *DB) tableSQL(table string) (string, error) {
	var ddl string
	err := d.conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ddl, nil
}

// rebuildEdgesUniqueKey migrates the edges unique key from
// UNIQUE(source_id,target_id,kind) to UNIQUE(source_id,target_id,kind,line,col)
// so one source can hold many call-site edges to the same target. line/col are
// made NOT NULL DEFAULT 0 so NULLs cannot bypass the constraint.
//
// SQLite cannot ALTER constraints, so the table is rebuilt: edges_new ← copy
// (COALESCE old NULLs to 0) → DROP → RENAME, then the edges indexes are
// recreated. PRAGMA foreign_keys cannot be toggled inside a transaction, so
// this runs on the raw connection outside any tx (Open calls it before any
// transaction starts).
func (d *DB) rebuildEdgesUniqueKey() error {
	ddl, err := d.tableSQL("edges")
	if err != nil {
		return fmt.Errorf("edges rebuild: read ddl: %w", err)
	}
	hasOld := strings.Contains(ddl, "UNIQUE(source_id, target_id, kind)")
	hasNew := strings.Contains(ddl, "UNIQUE(source_id, target_id, kind, line, col)")
	if !hasOld || hasNew {
		return nil
	}

	if _, err := d.conn.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("edges rebuild: disable foreign_keys: %w", err)
	}
	defer func() {
		// Re-enable FK enforcement on this connection; the DSN also sets it for
		// every new connection, so a failure here cannot leave FKs off.
		_, _ = d.conn.Exec(`PRAGMA foreign_keys=ON`)
	}()

	for _, q := range []string{
		`CREATE TABLE edges_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
			target_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			file TEXT,
			line INTEGER NOT NULL DEFAULT 0,
			col INTEGER NOT NULL DEFAULT 0,
			provenance TEXT,
			metadata TEXT,
			UNIQUE(source_id, target_id, kind, line, col)
		)`,
		`INSERT INTO edges_new (id, source_id, target_id, kind, file, line, col, provenance, metadata)
		 SELECT id, source_id, target_id, kind, file, COALESCE(line, 0), COALESCE(col, 0), provenance, metadata
		 FROM edges`,
		`DROP TABLE edges`,
		`ALTER TABLE edges_new RENAME TO edges`,
		`CREATE INDEX IF NOT EXISTS idx_edges_source ON edges(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_provenance ON edges(provenance)`,
	} {
		if _, err := d.conn.Exec(q); err != nil {
			return fmt.Errorf("edges rebuild: %w", err)
		}
	}
	return nil
}

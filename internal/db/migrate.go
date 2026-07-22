package db

import (
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

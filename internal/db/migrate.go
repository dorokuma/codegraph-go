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

// tableExists reports whether a table with the given name exists.
func (d *DB) tableExists(table string) (bool, error) {
	var name string
	err := d.conn.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// edgesUniqueKeyCols returns the column set of the unique index backing the
// UNIQUE(...) constraint on the given table (nil when the table has no unique
// index). The pre-A3 key is 3 columns (source_id,target_id,kind); the A3 key
// is 5 columns (source_id,target_id,kind,line,col). Introspecting the index
// columns is robust against DDL text drift, unlike substring matching on the
// stored CREATE TABLE sql.
func (d *DB) edgesUniqueKeyCols(table string) ([]string, error) {
	rows, err := d.conn.Query(`PRAGMA index_list("` + table + `")`)
	if err != nil {
		return nil, fmt.Errorf("pragma index_list(%s): %w", table, err)
	}
	// Collect unique index names first, then close the rows: the pool has a
	// single connection (MaxOpenConns(1)), so probing indexes while rows is
	// still open would deadlock.
	var uniq []string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pragma index_list(%s): %w", table, err)
		}
		if unique != 0 {
			uniq = append(uniq, name)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("pragma index_list(%s): %w", table, err)
	}
	rows.Close()
	// edges has at most one unique index (the UNIQUE constraint); first wins.
	for _, name := range uniq {
		cols, cerr := d.indexColumns(name)
		if cerr != nil {
			return nil, cerr
		}
		return cols, nil
	}
	return nil, nil
}

// indexColumns returns the column names of an index via PRAGMA index_info.
func (d *DB) indexColumns(idxName string) ([]string, error) {
	rows, err := d.conn.Query(`PRAGMA index_info("` + idxName + `")`)
	if err != nil {
		return nil, fmt.Errorf("pragma index_info(%s): %w", idxName, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var seqno, cid int
		var col sql.NullString
		if err := rows.Scan(&seqno, &cid, &col); err != nil {
			return nil, fmt.Errorf("pragma index_info(%s): %w", idxName, err)
		}
		if col.Valid {
			out = append(out, col.String)
		}
	}
	return out, rows.Err()
}

// isNewEdgeKey reports whether cols is the A3 5-column unique key
// (source_id,target_id,kind,line,col). The pre-A3 key has only 3 columns.
func isNewEdgeKey(cols []string) bool {
	if len(cols) != 5 {
		return false
	}
	want := map[string]bool{"source_id": true, "target_id": true, "kind": true, "line": true, "col": true}
	for _, c := range cols {
		if !want[c] {
			return false
		}
	}
	return true
}

// recoverEdgesRebuild completes an edges rebuild interrupted by a crash in
// pre-transaction builds. It MUST run before schema.sql is applied: a crash
// between DROP TABLE edges and RENAME leaves edges_new as the only copy of
// the edge data, and schema.sql would otherwise recreate an empty edges,
// orphaning every row. State is probed by table existence, not DDL text:
//   - edges_new exists AND edges exists → interrupted between CREATE and
//     DROP; edges is authoritative, so the stale copy is dropped and the
//     regular migration (rebuildEdgesUniqueKey) re-runs afterwards.
//   - edges_new exists AND edges missing → interrupted between DROP and
//     RENAME; edges_new holds the only copy, so it is promoted (RENAME +
//     lookup indexes).
//   - no edges_new → nothing to recover.
func (d *DB) recoverEdgesRebuild() error {
	en, err := d.tableExists("edges_new")
	if err != nil {
		return fmt.Errorf("edges rebuild recovery: probe edges_new: %w", err)
	}
	if !en {
		return nil
	}
	live, err := d.tableExists("edges")
	if err != nil {
		return fmt.Errorf("edges rebuild recovery: probe edges: %w", err)
	}
	if live {
		if _, err := d.conn.Exec(`DROP TABLE IF EXISTS edges_new`); err != nil {
			return fmt.Errorf("edges rebuild recovery: drop stale edges_new: %w", err)
		}
		return nil
	}
	return d.promoteEdgesNew()
}

// promoteEdgesNew completes a rebuild interrupted right after DROP TABLE
// edges: edges_new holds the only copy of the edge data. It is renamed to
// edges and the lookup indexes are recreated (they were dropped with the old
// table) — all in ONE transaction, matching rebuildEdgesUniqueKey's style
// (G3). No FK toggle is needed on this path: it performs no DROP, and the
// RENAME + CREATE INDEX statements are valid inside a transaction with
// foreign_keys=ON. A mid-run failure rolls back and leaves edges_new in
// place, so recoverEdgesRebuild can retry the promotion on the next Open —
// never a half-promoted state. If the promoted table still carries the old
// 3-column unique key, rebuildEdgesUniqueKey migrates it afterwards.
func (d *DB) promoteEdgesNew() error {
	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("edges rebuild recovery: begin tx: %w", err)
	}
	queries := []struct {
		label string
		sql   string
	}{
		{"promote edges_new", `ALTER TABLE edges_new RENAME TO edges`},
		{"index after promote", `CREATE INDEX IF NOT EXISTS idx_edges_source ON edges(source_id)`},
		{"index after promote", `CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target_id)`},
		{"index after promote", `CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind)`},
		{"index after promote", `CREATE INDEX IF NOT EXISTS idx_edges_provenance ON edges(provenance)`},
	}
	for _, q := range queries {
		if _, err := tx.Exec(q.sql); err != nil {
			_ = tx.Rollback()
			// Do NOT drop edges_new here: it holds the only copy of the edge
			// data (edges does not exist yet), and the rollback has already
			// undone the RENAME — recovery retries on the next Open.
			return fmt.Errorf("edges rebuild recovery: %s: %w", q.label, err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("edges rebuild recovery: commit: %w", err)
	}
	return nil
}

// rebuildEdgesUniqueKey migrates the edges unique key from
// UNIQUE(source_id,target_id,kind) to UNIQUE(source_id,target_id,kind,line,col)
// so one source can hold many call-site edges to the same target. line/col are
// made NOT NULL DEFAULT 0 so NULLs cannot bypass the constraint.
//
// SQLite cannot ALTER constraints, so the table is rebuilt: edges_new ← copy
// (COALESCE old NULLs to 0) → DROP → RENAME, then the edges indexes are
// recreated. The whole sequence runs in ONE transaction with foreign_keys=OFF
// (SQLite only allows DDL inside a transaction while FK enforcement is off,
// and PRAGMA foreign_keys cannot be toggled inside a transaction, so it is
// toggled around the tx on the raw connection). A crash mid-run rolls back to
// the untouched old table, and any failure path drops the half-built
// edges_new; leftover copies from pre-transaction builds are already handled
// by recoverEdgesRebuild before schema.sql in Open. Entry is idempotent: the
// state probe uses PRAGMA index introspection on the unique index column set
// (3 columns = old key, 5 columns = new key), never DDL substring matching.
func (d *DB) rebuildEdgesUniqueKey() (retErr error) {
	// No recovery should have left a copy behind; a leftover here is never
	// authoritative (recoverEdgesRebuild promoted it pre-schema if needed).
	if _, err := d.conn.Exec(`DROP TABLE IF EXISTS edges_new`); err != nil {
		return fmt.Errorf("edges rebuild: drop stale edges_new: %w", err)
	}

	// State probe: is the A3 key already effective on the live table?
	cols, err := d.edgesUniqueKeyCols("edges")
	if err != nil {
		return fmt.Errorf("edges rebuild: probe unique key: %w", err)
	}
	if isNewEdgeKey(cols) || cols == nil {
		// Already migrated, or no edges table at all (fresh schema.sql).
		return nil
	}

	if _, err := d.conn.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("edges rebuild: disable foreign_keys: %w", err)
	}
	defer func() {
		// Re-enable FK enforcement on this connection. The DSN also sets it
		// for NEW connections, but this one is pooled for the process
		// lifetime (MaxOpenConns(1)) — if the re-enable failed and was
		// swallowed, CASCADE would silently stop working (deleting a node
		// would orphan its edges/unresolved_refs). Surface the failure: it
		// propagates through ensureSchema out of Open and aborts startup
		// (the next Open starts with a fresh connection that re-enables FK).
		if _, err := d.conn.Exec(`PRAGMA foreign_keys=ON`); err != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%v (additionally failed to re-enable foreign_keys: %w)", retErr, err)
			} else {
				retErr = fmt.Errorf("edges rebuild: re-enable foreign_keys: %w", err)
			}
		}
	}()

	// One transaction for the whole DDL/DML sequence: either the new key is
	// fully in place or the old table is untouched. No other write happens
	// between COMMIT and the deferred foreign_keys=ON above.
	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("edges rebuild: begin tx: %w", err)
	}
	abort := func(cause error) error {
		_ = tx.Rollback()
		// Leave no half-built table behind on any failure path.
		_, _ = d.conn.Exec(`DROP TABLE IF EXISTS edges_new`)
		return cause
	}
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
		if _, err := tx.Exec(q); err != nil {
			return abort(fmt.Errorf("edges rebuild: %w", err))
		}
	}
	if err := tx.Commit(); err != nil {
		return abort(fmt.Errorf("edges rebuild: commit: %w", err))
	}
	return nil
}

package db

import (
	"fmt"
	"strings"
)

func stringsContainsNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// IndexSchemaRevision bumps whenever on-disk graph semantics change in a way
// that old rows become wrong/incomplete (e.g. route→handler references).
// On mismatch the server wipes symbol data and does a full reindex.
const IndexSchemaRevision = "15"

const metaSchemaKey = "index_schema_revision"

// NeedsRebuild reports whether the on-disk index was built with an older
// schema revision and must be wiped + fully reindexed.
// meta table is created by schema.sql / Open — this method only reads.
func (d *DB) NeedsRebuild() (bool, string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var cur string
	err := d.conn.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaSchemaKey).Scan(&cur)
	if err != nil {
		// missing row or missing table on very old DBs → rebuild
		return true, "(none)", nil
	}
	if cur != IndexSchemaRevision {
		return true, cur, nil
	}
	return false, cur, nil
}

// SetSchemaRevision records that the index matches the current extractor semantics.
func (d *DB) SetSchemaRevision() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`
		INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaSchemaKey, IndexSchemaRevision)
	return err
}

// WipeIndex deletes all nodes/edges/files so a full reindex can repopulate.
// Schema and meta (except we clear schema revision until SetSchemaRevision) stay.
func (d *DB) WipeIndex() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Order matters for FTS external-content triggers and FK cascades.
	// unresolved_refs references nodes; clear it first (or rely on CASCADE).
	for _, q := range []string{
		`DELETE FROM unresolved_refs`,
		`DELETE FROM edges`,
		`DELETE FROM nodes`,
		`DELETE FROM files`,
	} {
		if _, err := tx.Exec(q); err != nil {
			// unresolved_refs may be absent on extremely old embeds mid-migrate
			if q == `DELETE FROM unresolved_refs` && stringsContainsNoSuchTable(err) {
				continue
			}
			return fmt.Errorf("wipe: %s: %w", q, err)
		}
	}
	// Rebuild empty FTS
	if _, err := tx.Exec(`INSERT INTO nodes_fts(nodes_fts) VALUES('rebuild')`); err != nil {
		// non-fatal on empty
		_ = err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// SchemaRevision returns the constant current schema revision string.
func SchemaRevision() string { return IndexSchemaRevision }

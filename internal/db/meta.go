package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func stringsContainsNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// IndexSchemaRevision bumps whenever on-disk graph semantics change in a way
// that old rows become wrong/incomplete (e.g. route→handler references,
// call-edge line numbers switched to call-site, anonymous-closure calls now
// captured). On mismatch the server wipes symbol data and does a full reindex.
// Bumped 16→17: index stores workdir-relative paths (portable; fixes affected).
// Bumped 17→18: edges uniqueness now includes line+col (multi call-site edges);
// old single-edge-per-pair indexes are stale and need wipe+rebuild.
// Bumped 18→19: 0.9.0 extractor semantics — Go interface methods as
// kind=signature nodes, Go multi-line import edges, JS require() calls,
// call-site line-number fixes, same-name disambiguation. Old rows are
// wrong/incomplete under the new extractor, and content_hash skips
// re-extraction of unchanged files, so the wipe+rebuild must be forced.
const IndexSchemaRevision = "19"

const metaSchemaKey = "index_schema_revision"

// NeedsRebuild reports whether the on-disk index was built with an older
// schema revision and must be wiped + fully reindexed.
// meta table is created by schema.sql / Open — this method only reads.
func (d *DB) NeedsRebuild() (bool, string, error) {
	return d.NeedsRebuildContext(context.Background())
}

// NeedsRebuildContext is the context-aware variant of NeedsRebuild.
func (d *DB) NeedsRebuildContext(ctx context.Context) (bool, string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var cur string
	err := d.conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaSchemaKey).Scan(&cur)
	if err != nil {
		// Missing row or missing table on very old DBs → rebuild.
		if err == sql.ErrNoRows || stringsContainsNoSuchTable(err) {
			return true, "(none)", nil
		}
		// Any other error (closed connection, lock contention, …) must NOT
		// trigger a wipe: returning (true, …) would wipe the index on a
		// transient failure. Surface the error instead.
		return false, "", err
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
// Schema, meta, and the schema-revision key are PRESERVED: WipeIndex clears
// only symbol data (facts and meta rows survive, see TestFactsSurviveWipeIndex).
// The revision is updated — by SetSchemaRevision, after a rebuild succeeded —
// never here: a wiped-but-unrebuilt index still carries its old revision
// value, so after a schema bump NeedsRebuild keeps reporting true until the
// rebuild completes and the new revision is recorded (a failed rebuild is
// detected on the next startup instead of trusting an empty/half index).
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
		return fmt.Errorf("wipe: rebuild nodes_fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// SchemaRevision returns the constant current schema revision string.
func SchemaRevision() string { return IndexSchemaRevision }

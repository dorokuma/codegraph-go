package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateUnresolvedRefsAttemptsColumn simulates a pre-0.9.12 on-disk DB
// whose unresolved_refs table predates the attempts column and verifies that
// Open adds the column in place: existing rows (and facts, whose table is
// rebuilt by rebuildFactsHashKey on the same path) survive, attempts defaults
// to 0, and the index stays valid — the column migration must NOT bump or
// invalidate IndexSchemaRevision (no wipe+rebuild).
func TestMigrateUnresolvedRefsAttemptsColumn(t *testing.T) {
	dir := t.TempDir()
	codegraph := filepath.Join(dir, ".codegraph")
	if err := os.MkdirAll(codegraph, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(codegraph, "codegraph.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Old-shape schema: current revision recorded, unresolved_refs WITHOUT
	// attempts, one parked ref row, facts in its pre-idx_facts_hash_target
	// shape (table-level UNIQUE(content_hash)) with one live fact.
	if _, err := raw.Exec(`
		CREATE TABLE meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		INSERT INTO meta(key, value) VALUES('index_schema_revision', '19');
		CREATE TABLE nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			file TEXT NOT NULL,
			line INTEGER NOT NULL,
			end_line INTEGER,
			body TEXT,
			language TEXT,
			UNIQUE(file, line, kind, name)
		);
		CREATE TABLE edges (
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
		);
		CREATE TABLE files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT NOT NULL UNIQUE,
			size INTEGER,
			mtime REAL,
			indexed_at REAL
		);
		CREATE TABLE unresolved_refs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			from_node INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			reference_name TEXT NOT NULL,
			reference_kind TEXT NOT NULL,
			line INTEGER NOT NULL,
			col INTEGER NOT NULL DEFAULT 0,
			file_path TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT 'unknown',
			status TEXT NOT NULL DEFAULT 'pending',
			name_tail TEXT NOT NULL DEFAULT '',
			candidates TEXT,
			UNIQUE(from_node, reference_name, reference_kind, line, col)
		);
		CREATE TABLE facts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			target_file TEXT NOT NULL,
			target_symbol TEXT,
			target_line INTEGER,
			content TEXT NOT NULL,
			content_hash TEXT NOT NULL UNIQUE,
			author TEXT,
			status TEXT NOT NULL DEFAULT 'active',
			superseded_by INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO nodes(kind, name, file, line, body, language)
		VALUES ('function', 'Caller', '/a.go', 1, 'body', 'go');
		INSERT INTO unresolved_refs(from_node, reference_name, reference_kind, line, col, file_path, language, status, name_tail)
		VALUES (1, 'Fatal', 'calls', 2, 0, '/a.go', 'go', 'failed', 'Fatal');
		INSERT INTO facts(target_file, target_symbol, target_line, content, content_hash, created_at, updated_at)
		VALUES ('/a.go', 'Caller', 1, 'note', 'hash-1', 1, 1);
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	defer database.Close()

	// Column added in place
	have, err := database.tableColumns("unresolved_refs")
	if err != nil {
		t.Fatal(err)
	}
	if !have["attempts"] {
		t.Fatalf("migrated unresolved_refs missing attempts (have %v)", have)
	}

	// Existing row intact with attempts defaulting to 0
	var name, status string
	var attempts int
	if err := database.conn.QueryRow(
		`SELECT reference_name, status, attempts FROM unresolved_refs WHERE id = 1`,
	).Scan(&name, &status, &attempts); err != nil {
		t.Fatalf("parked ref row lost during migration: %v", err)
	}
	if name != "Fatal" || status != "failed" || attempts != 0 {
		t.Fatalf("row mismatch: name=%s status=%s attempts=%d", name, status, attempts)
	}

	// Facts survived the same migration (facts table rebuild path untouched)
	var content string
	if err := database.conn.QueryRow(`SELECT content FROM facts WHERE id = 1`).Scan(&content); err != nil {
		t.Fatalf("fact row lost during migration: %v", err)
	}
	if content != "note" {
		t.Fatalf("fact content mismatch: %q", content)
	}

	// The column migration must not invalidate the index: revision recorded
	// as current before Open still reports no rebuild needed.
	need, rev, err := database.NeedsRebuild()
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatalf("adding attempts must not trigger a rebuild (rev=%q)", rev)
	}
	if rev != "19" {
		t.Fatalf("revision changed during migration: %q", rev)
	}
}

// TestMarkUnresolvedFailedAttemptsAbandon covers the retry-cap semantics of
// MarkUnresolvedFailed: attempts increment per failed attempt, the row flips
// to 'abandoned' at the cap, and abandoned rows disappear from every
// pending/failed retry query while staying in the table for audit.
func TestMarkUnresolvedFailedAttemptsAbandon(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	fromID, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "caller", File: "a.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	rid, err := database.InsertUnresolvedRef(&UnresolvedRef{
		FromNode:      fromID,
		ReferenceName: "Fatal",
		ReferenceKind: EdgeCalls,
		Line:          2,
		FilePath:      "a.go",
		Language:      "go",
		Status:        "pending",
		NameTail:      "Fatal",
	})
	if err != nil {
		t.Fatal(err)
	}

	const cap = 3
	for attempt := 1; attempt <= cap; attempt++ {
		abandoned, err := database.MarkUnresolvedFailed(rid, "Fatal", cap)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		var status string
		var attempts int
		if err := database.conn.QueryRow(
			`SELECT status, attempts FROM unresolved_refs WHERE id = ?`, rid,
		).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != attempt {
			t.Fatalf("attempt %d: attempts=%d", attempt, attempts)
		}
		if attempt < cap {
			if abandoned || status != "failed" {
				t.Fatalf("attempt %d below cap: abandoned=%v status=%s", attempt, abandoned, status)
			}
			if n, _ := database.CountUnresolvedRefs("failed"); n != 1 {
				t.Fatalf("attempt %d: failed count=%d", attempt, n)
			}
		} else {
			if !abandoned || status != "abandoned" {
				t.Fatalf("attempt %d at cap: abandoned=%v status=%s", attempt, abandoned, status)
			}
		}
	}

	// Kept for audit, but out of every retry selection.
	if n, _ := database.CountUnresolvedRefs("abandoned"); n != 1 {
		t.Fatalf("abandoned count=%d, want 1", n)
	}
	if n, _ := database.CountUnresolvedRefs("failed"); n != 0 {
		t.Fatalf("failed count=%d after abandon, want 0", n)
	}
	if refs, _ := database.ListUnresolvedRefs("", "failed"); len(refs) != 0 {
		t.Fatalf("failed retry query still selects the abandoned row: %v", refs)
	}
	if refs, _ := database.ListUnresolvedRefsByNames([]string{"Fatal"}, []string{"pending", "failed"}); len(refs) != 0 {
		t.Fatalf("name retry query still selects the abandoned row: %v", refs)
	}
	if refs, _ := database.ListUnresolvedRefsByFiles([]string{"a.go"}, "failed"); len(refs) != 0 {
		t.Fatalf("file retry query still selects the abandoned row: %v", refs)
	}
	if refs, _ := database.ListUnresolvedRefsEmptyTail([]string{"pending", "failed"}); len(refs) != 0 {
		t.Fatalf("empty-tail retry query still selects the abandoned row: %v", refs)
	}
	if refs, _ := database.ListUnresolvedRefs("", ""); len(refs) != 1 {
		t.Fatalf("abandoned row must be kept for audit, got %d rows", len(refs))
	}

	// Marking a missing row stays a tolerated no-op.
	if abandoned, err := database.MarkUnresolvedFailed(99999, "x", cap); err != nil || abandoned {
		t.Fatalf("missing row: abandoned=%v err=%v", abandoned, err)
	}
}

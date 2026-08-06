package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// ---- A1: single-writer lock ----

func TestOpenSingleWriterLock(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Second Open on the same path must fail with an in-use error.
	_, err = Open(dir)
	if err == nil {
		t.Fatal("second open on same path should fail (single writer)")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("expected in-use error, got: %v", err)
	}

	// When a daemon pidfile is present the error carries pid/version info.
	pidfile := filepath.Join(dir, ".codegraph", "daemon.pid")
	if err := os.WriteFile(pidfile, []byte(`{"pid": 4242, "version": "0.8.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(dir)
	if err == nil {
		t.Fatal("second open should still fail")
	}
	if !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), "0.8.1") {
		t.Fatalf("in-use error should carry pid/version from daemon.pid, got: %v", err)
	}
	if err := os.Remove(pidfile); err != nil {
		t.Fatal(err)
	}

	// Close releases the lock; Open succeeds again.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database2, err := Open(dir)
	if err != nil {
		t.Fatalf("open after close should succeed: %v", err)
	}
	if err := database2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSingleWriterLockDifferentDirs(t *testing.T) {
	// Independent projects must not block each other.
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("independent dir open failed: %v", err)
	}
	defer b.Close()
}

// ---- A2: NeedsRebuild must not trigger wipe on non-missing errors ----

func TestNeedsRebuildClosedConnReturnsError(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Force a non-ErrNoRows / non-missing-table query error.
	if err := database.conn.Close(); err != nil {
		t.Fatal(err)
	}
	need, old, err := database.NeedsRebuild()
	if err == nil {
		t.Fatal("expected error from closed connection")
	}
	if need {
		t.Fatal("closed connection must NOT trigger rebuild/wipe")
	}
	if old != "" {
		t.Fatalf("expected empty old revision on error, got %q", old)
	}
}

func TestNeedsRebuildMissingMetaTable(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.conn.Exec(`DROP TABLE meta`); err != nil {
		t.Fatal(err)
	}
	need, old, err := database.NeedsRebuild()
	if err != nil {
		t.Fatalf("missing meta table should not error: %v", err)
	}
	if !need || old != "(none)" {
		t.Fatalf("want (true, \"(none)\") for missing table, got (%v, %q)", need, old)
	}
}

// ---- A3: edges uniqueness includes line+col (multi call-site edges) ----

func TestEdgeMultiCallSites(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "caller", File: "/a.go", Line: 1})
	id2, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "callee", File: "/b.go", Line: 1})

	// Three call sites from caller → callee.
	database.UpsertEdge(&Edge{SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 5, Col: 1})
	database.UpsertEdge(&Edge{SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 9, Col: 1})
	database.UpsertEdge(&Edge{SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 13, Col: 1})

	// GetCallers reports distinct caller nodes, not call sites.
	callers, err := database.GetCallers(id2)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0].Name != "caller" {
		t.Fatalf("want 1 distinct caller, got %d", len(callers))
	}

	// GetImpact counts call sites per file.
	impact, err := database.GetImpact(id2)
	if err != nil {
		t.Fatal(err)
	}
	if impact["/a.go"] != 3 {
		t.Fatalf("want 3 call-site hits for /a.go, got %d", impact["/a.go"])
	}

	// Per-call-site rows visible via GetIncomingEdges.
	incoming, err := database.GetIncomingEdges(id2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(incoming) != 3 {
		t.Fatalf("want 3 edge rows, got %d", len(incoming))
	}

	// Re-upserting the same site merges (still 3 rows).
	database.UpsertEdge(&Edge{SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 5, Col: 1})
	incoming, _ = database.GetIncomingEdges(id2, nil)
	if len(incoming) != 3 {
		t.Fatalf("duplicate call site should merge, got %d", len(incoming))
	}

	// Same pair, same kind, different line → distinct edge row.
	database.UpsertEdge(&Edge{SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 20})
	incoming, _ = database.GetIncomingEdges(id2, nil)
	if len(incoming) != 4 {
		t.Fatalf("new call site should add a row, got %d", len(incoming))
	}
}

// TestEdgesUniqueKeyMigration simulates a pre-A3 database whose edges unique
// key is UNIQUE(source_id,target_id,kind) with NULLable line/col, then verifies
// Open migrates the table: NOT NULL DEFAULT 0 line/col, new 5-column unique
// key, old rows preserved (NULLs → 0), and multi call-site inserts allowed.
func TestEdgesUniqueKeyMigration(t *testing.T) {
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
	if _, err := raw.Exec(`
		CREATE TABLE nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL, name TEXT NOT NULL, file TEXT NOT NULL,
			line INTEGER NOT NULL, end_line INTEGER, body TEXT, language TEXT,
			UNIQUE(file, line, kind, name)
		);
		INSERT INTO nodes(kind, name, file, line) VALUES ('function','caller','/a.go',1), ('function','callee','/b.go',1);
		CREATE TABLE edges (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER, target_id INTEGER,
			kind TEXT NOT NULL, file TEXT, line INTEGER, col INTEGER,
			provenance TEXT, metadata TEXT,
			UNIQUE(source_id, target_id, kind)
		);
		INSERT INTO edges (source_id, target_id, kind, file, line) VALUES (1, 2, 'calls', 'a.go', 5);
		INSERT INTO edges (source_id, target_id, kind, file, line) VALUES (2, 1, 'calls', 'b.go', NULL);
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

	ddl, err := database.tableSQL("edges")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "UNIQUE(source_id, target_id, kind, line, col)") {
		t.Fatalf("edges not migrated to new unique key; ddl=%s", ddl)
	}

	// Old rows preserved; NULL line → 0.
	edges, err := database.GetIncomingEdges(2, nil) // target 2 ← source 1 (line 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Line != 5 || edges[0].Col != 0 {
		t.Fatalf("row 1 (1→2): want line=5 col=0, got %+v", edges)
	}
	edges2, err := database.GetIncomingEdges(1, nil) // target 1 ← source 2 (line NULL)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges2) != 1 || edges2[0].Line != 0 || edges2[0].Col != 0 {
		t.Fatalf("row 2 (2→1): want line=0 col=0 (NULL coalesced), got %+v", edges2)
	}

	// Multi call sites now allowed after migration.
	if _, err := database.UpsertEdge(&Edge{SourceID: 1, TargetID: 2, Kind: EdgeCalls, File: "a.go", Line: 99}); err != nil {
		t.Fatal(err)
	}
	incoming, err := database.GetIncomingEdges(2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(incoming) != 2 {
		t.Fatalf("want 2 call-site rows after migration, got %d", len(incoming))
	}
}

// TestEdgesUniqueKeyMigrationLeftoverEdgesNew simulates a crash between
// CREATE TABLE edges_new and DROP TABLE edges in pre-transaction builds:
// edges_new exists with a copy of the rows while edges still carries the old
// 3-column key. Open must drop the leftover, redo the migration, keep every
// row, and end with the new 5-column key effective (M1).
func TestEdgesUniqueKeyMigrationLeftoverEdgesNew(t *testing.T) {
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
	// edges_new mirrors what the interrupted pre-transaction migration left:
	// the new 5-column key structure with the copied rows, while edges is
	// untouched with the old key.
	if _, err := raw.Exec(`
		CREATE TABLE nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL, name TEXT NOT NULL, file TEXT NOT NULL,
			line INTEGER NOT NULL, end_line INTEGER, body TEXT, language TEXT,
			UNIQUE(file, line, kind, name)
		);
		INSERT INTO nodes(kind, name, file, line) VALUES ('function','caller','/a.go',1), ('function','callee','/b.go',1);
		CREATE TABLE edges (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER, target_id INTEGER,
			kind TEXT NOT NULL, file TEXT, line INTEGER, col INTEGER,
			provenance TEXT, metadata TEXT,
			UNIQUE(source_id, target_id, kind)
		);
		INSERT INTO edges (source_id, target_id, kind, file, line) VALUES (1, 2, 'calls', 'a.go', 5);
		INSERT INTO edges (source_id, target_id, kind, file, line) VALUES (2, 1, 'calls', 'b.go', NULL);
		CREATE TABLE edges_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER, target_id INTEGER,
			kind TEXT NOT NULL, file TEXT,
			line INTEGER NOT NULL DEFAULT 0, col INTEGER NOT NULL DEFAULT 0,
			provenance TEXT, metadata TEXT,
			UNIQUE(source_id, target_id, kind, line, col)
		);
		INSERT INTO edges_new (id, source_id, target_id, kind, file, line, col, provenance, metadata)
			SELECT id, source_id, target_id, kind, file, COALESCE(line,0), COALESCE(col,0), provenance, metadata FROM edges;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open db with leftover edges_new: %v", err)
	}
	defer database.Close()

	ddl, err := database.tableSQL("edges")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "UNIQUE(source_id, target_id, kind, line, col)") {
		t.Fatalf("edges not migrated to new unique key; ddl=%s", ddl)
	}
	// The leftover copy must be gone.
	en, err := database.tableExists("edges_new")
	if err != nil || en {
		t.Fatalf("leftover edges_new must be cleaned up: exists=%v err=%v", en, err)
	}
	// Rows preserved; NULL line coalesced to 0.
	edges, err := database.GetIncomingEdges(2, nil)
	if err != nil || len(edges) != 1 || edges[0].Line != 5 || edges[0].Col != 0 {
		t.Fatalf("row 1 (1→2): want line=5 col=0, got %+v err=%v", edges, err)
	}
	edges2, err := database.GetIncomingEdges(1, nil)
	if err != nil || len(edges2) != 1 || edges2[0].Line != 0 || edges2[0].Col != 0 {
		t.Fatalf("row 2 (2→1): want line=0 col=0 (NULL coalesced), got %+v err=%v", edges2, err)
	}
}

// TestEdgesUniqueKeyMigrationPromotesEdgesNew simulates a crash between
// DROP TABLE edges and RENAME (pre-transaction builds): edges is gone and
// edges_new holds the only copy of the data. Open must promote edges_new
// (RENAME + lookup indexes) before schema.sql can recreate an empty edges,
// keeping every row under the new key (M1).
func TestEdgesUniqueKeyMigrationPromotesEdgesNew(t *testing.T) {
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
	// Crash state: only edges_new exists, holding the data (no edges table).
	if _, err := raw.Exec(`
		CREATE TABLE nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL, name TEXT NOT NULL, file TEXT NOT NULL,
			line INTEGER NOT NULL, end_line INTEGER, body TEXT, language TEXT,
			UNIQUE(file, line, kind, name)
		);
		INSERT INTO nodes(kind, name, file, line) VALUES ('function','caller','/a.go',1), ('function','callee','/b.go',1);
		CREATE TABLE edges_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
			target_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
			kind TEXT NOT NULL, file TEXT,
			line INTEGER NOT NULL DEFAULT 0, col INTEGER NOT NULL DEFAULT 0,
			provenance TEXT, metadata TEXT,
			UNIQUE(source_id, target_id, kind, line, col)
		);
		INSERT INTO edges_new (source_id, target_id, kind, file, line) VALUES (1, 2, 'calls', 'a.go', 5);
		INSERT INTO edges_new (source_id, target_id, kind, file, line) VALUES (2, 1, 'calls', 'b.go', 3);
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open db with promoted candidate: %v", err)
	}
	defer database.Close()

	ddl, err := database.tableSQL("edges")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "UNIQUE(source_id, target_id, kind, line, col)") {
		t.Fatalf("edges not promoted with new unique key; ddl=%s", ddl)
	}
	// Rows preserved through the promotion.
	edges, err := database.GetIncomingEdges(2, nil)
	if err != nil || len(edges) != 1 || edges[0].Line != 5 {
		t.Fatalf("row 1 (1→2): want line=5, got %+v err=%v", edges, err)
	}
	edges2, err := database.GetIncomingEdges(1, nil)
	if err != nil || len(edges2) != 1 || edges2[0].Line != 3 {
		t.Fatalf("row 2 (2→1): want line=3, got %+v err=%v", edges2, err)
	}
	// Lookup indexes dropped with the old table must be rebuilt.
	for _, idx := range []string{"idx_edges_source", "idx_edges_target", "idx_edges_kind", "idx_edges_provenance"} {
		var name string
		if err := database.conn.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name); err != nil {
			t.Fatalf("index %s missing after promotion: %v", idx, err)
		}
	}
	en, err := database.tableExists("edges_new")
	if err != nil || en {
		t.Fatalf("edges_new must be gone after promotion: exists=%v err=%v", en, err)
	}
}

// TestCloseIdempotent: a second Close must be a no-op returning nil (no
// "sql: database is closed"), and the flock is released exactly once so a
// fresh Open wins (S2).
func TestCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("second close must be a no-op, got: %v", err)
	}
	database2, err := Open(dir)
	if err != nil {
		t.Fatalf("open after double close should succeed: %v", err)
	}
	if err := database2.Close(); err != nil {
		t.Fatal(err)
	}
}

// ---- A7: snapshot truncation ----

func TestGraphSnapshotTruncation(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	oldCap := graphSnapshotCap
	graphSnapshotCap = 5
	defer func() { graphSnapshotCap = oldCap }()

	for i := 0; i < 10; i++ {
		if _, err := database.UpsertNode(&Node{
			Kind: KindFunction, Name: fmt.Sprintf("f%d", i), File: "/a.go", Line: i + 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := database.GetGraphSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Truncated {
		t.Fatal("expected Truncated flag when nodes exceed cap")
	}
	if len(snap.Nodes) != 5 {
		t.Fatalf("want 5 capped nodes, got %d", len(snap.Nodes))
	}

	// Under the cap: no truncation flag.
	graphSnapshotCap = 100
	snap, err = database.GetGraphSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Truncated {
		t.Fatal("unexpected truncation under cap")
	}
	if len(snap.Nodes) != 10 {
		t.Fatalf("want 10 nodes, got %d", len(snap.Nodes))
	}
}

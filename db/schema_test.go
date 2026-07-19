package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSchemaRevisionIs15(t *testing.T) {
	if SchemaRevision() != "15" {
		t.Fatalf("IndexSchemaRevision = %q, want 15", SchemaRevision())
	}
}

func TestSchemaHasV7Columns(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	want := map[string][]string{
		"nodes": {
			"qualified_name", "signature", "docstring",
			"start_column", "end_column", "visibility", "is_exported", "return_type",
		},
		"edges": {"col", "provenance", "metadata"},
		"files": {"content_hash", "language", "node_count"},
		"unresolved_refs": {
			"from_node", "reference_name", "reference_kind", "line", "col",
			"file_path", "language", "status", "name_tail", "candidates",
		},
	}
	for table, cols := range want {
		have, err := database.tableColumns(table)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for _, c := range cols {
			if !have[c] {
				t.Errorf("table %s missing column %s (have %v)", table, c, have)
			}
		}
	}
}

func TestUpsertNodeQualifiedNameAndEdgeProvenance(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	srcID, err := database.UpsertNode(&Node{
		Kind:          KindFunction,
		Name:          "Caller",
		File:          "/pkg/a.go",
		Line:          10,
		EndLine:       20,
		Body:          "func Caller() { Callee() }",
		Language:      "go",
		QualifiedName: "pkg.Caller",
		Signature:     "func Caller()",
		Docstring:     "calls Callee",
		StartColumn:   1,
		EndColumn:     12,
		Visibility:    "public",
		IsExported:    true,
		ReturnType:    "",
	})
	if err != nil {
		t.Fatalf("upsert src: %v", err)
	}
	dstID, err := database.UpsertNode(&Node{
		Kind:          KindFunction,
		Name:          "Callee",
		File:          "/pkg/b.go",
		Line:          5,
		Language:      "go",
		QualifiedName: "pkg.Callee",
		Signature:     "func Callee()",
		IsExported:    true,
	})
	if err != nil {
		t.Fatalf("upsert dst: %v", err)
	}

	if _, err := database.UpsertEdge(&Edge{
		SourceID:   srcID,
		TargetID:   dstID,
		Kind:       EdgeCalls,
		File:       "/pkg/a.go",
		Line:       12,
		Col:        4,
		Provenance: "exact",
		Metadata:   `{"note":"step1"}`,
	}); err != nil {
		t.Fatalf("upsert edge: %v", err)
	}

	nodes, err := database.GetNodeByName("Caller")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("get Caller: err=%v n=%d", err, len(nodes))
	}
	n := nodes[0]
	if n.QualifiedName != "pkg.Caller" || n.Signature != "func Caller()" || !n.IsExported {
		t.Fatalf("node fields not persisted: %+v", n)
	}
	if n.StartColumn != 1 || n.Docstring != "calls Callee" {
		t.Fatalf("column/doc fields: %+v", n)
	}

	e, err := database.GetEdgeByEndpoints(srcID, dstID, EdgeCalls)
	if err != nil || e == nil {
		t.Fatalf("get edge: err=%v e=%v", err, e)
	}
	if e.Provenance != "exact" || e.Col != 4 || e.Metadata != `{"note":"step1"}` {
		t.Fatalf("edge fields: %+v", e)
	}
}

func TestUnresolvedRefsInsertAndWipe(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	fromID, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "f", File: "/a.go", Line: 1, Language: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertUnresolvedRef(&UnresolvedRef{
		FromNode:      fromID,
		ReferenceName: "missing",
		ReferenceKind: "calls",
		Line:          2,
		Col:           1,
		FilePath:      "/a.go",
		Language:      "go",
		Status:        "pending",
		NameTail:      "missing",
	}); err != nil {
		t.Fatalf("insert unresolved: %v", err)
	}
	n, err := database.CountUnresolvedRefs("pending")
	if err != nil || n != 1 {
		t.Fatalf("count pending: n=%d err=%v", n, err)
	}

	if err := database.WipeIndex(); err != nil {
		t.Fatal(err)
	}
	n, err = database.CountUnresolvedRefs("")
	if err != nil || n != 0 {
		t.Fatalf("after wipe want 0 unresolved, got %d err=%v", n, err)
	}
}

func TestUpsertFileRecordExtraFields(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	if err := database.UpsertFileRecord(&FileRecord{
		Path:        "/pkg/a.go",
		Size:        100,
		Mtime:       1.5,
		ContentHash: "abc123",
		Language:    "go",
		NodeCount:   3,
	}); err != nil {
		t.Fatal(err)
	}

	var hash, lang string
	var nodes int
	err := database.conn.QueryRow(
		`SELECT content_hash, language, node_count FROM files WHERE path = ?`,
		"/pkg/a.go",
	).Scan(&hash, &lang, &nodes)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "abc123" || lang != "go" || nodes != 3 {
		t.Fatalf("file fields hash=%s lang=%s nodes=%d", hash, lang, nodes)
	}
}

// TestOldDBUpgradesToV7 simulates a pre-v7 on-disk DB and checks Open migrates
// columns, creates unresolved_refs, and reports NeedsRebuild for logic bump.
func TestOldDBUpgradesToV7(t *testing.T) {
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
	// Minimal v6-shaped schema (no qualified_name / provenance / unresolved_refs).
	if _, err := raw.Exec(`
		CREATE TABLE meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		INSERT INTO meta(key, value) VALUES('index_schema_revision', '6');
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
			source_id INTEGER,
			target_id INTEGER,
			kind TEXT NOT NULL,
			file TEXT,
			line INTEGER,
			UNIQUE(source_id, target_id, kind)
		);
		CREATE TABLE files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT NOT NULL UNIQUE,
			size INTEGER,
			mtime REAL,
			indexed_at REAL
		);
		INSERT INTO nodes(kind, name, file, line, body, language)
		VALUES ('function', 'OldFn', '/old.go', 1, 'body', 'go');
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

	// Columns added
	have, err := database.tableColumns("nodes")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"qualified_name", "signature", "return_type"} {
		if !have[c] {
			t.Errorf("migrated nodes missing %s", c)
		}
	}
	haveE, err := database.tableColumns("edges")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"provenance", "metadata", "col"} {
		if !haveE[c] {
			t.Errorf("migrated edges missing %s", c)
		}
	}
	haveF, err := database.tableColumns("files")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"content_hash", "language", "node_count"} {
		if !haveF[c] {
			t.Errorf("migrated files missing %s", c)
		}
	}
	// unresolved_refs table exists
	if _, err := database.tableColumns("unresolved_refs"); err != nil {
		t.Fatalf("unresolved_refs missing after upgrade: %v", err)
	}

	// Logic mismatch → rebuild required
	need, old, err := database.NeedsRebuild()
	if err != nil {
		t.Fatal(err)
	}
	if !need || old != "6" {
		t.Fatalf("want rebuild from 6, got need=%v old=%s", need, old)
	}

	// Can write new fields on upgraded schema
	id, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "NewFn", File: "/n.go", Line: 1,
		QualifiedName: "pkg.NewFn", Language: "go",
	})
	if err != nil || id == 0 {
		t.Fatalf("upsert on upgraded db: id=%d err=%v", id, err)
	}
	nodes, err := database.GetNodeByName("NewFn")
	if err != nil || len(nodes) != 1 || nodes[0].QualifiedName != "pkg.NewFn" {
		t.Fatalf("read qualified_name after upgrade: %+v err=%v", nodes, err)
	}

	// Wipe (as main would on rebuild) succeeds including unresolved_refs
	if err := database.WipeIndex(); err != nil {
		t.Fatalf("wipe after upgrade: %v", err)
	}
	if err := database.SetSchemaRevision(); err != nil {
		t.Fatal(err)
	}
	need, _, err = database.NeedsRebuild()
	if err != nil || need {
		t.Fatalf("after set logic, need=%v err=%v", need, err)
	}
}

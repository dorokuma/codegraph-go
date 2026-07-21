package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cleanup := func() {
		database.Close()
	}
	return database, cleanup
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()

	// Check DB file exists
	dbPath := filepath.Join(dir, ".codegraph", "codegraph.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not found: %v", err)
	}
}

func TestUpsertNode(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	node := &Node{
		Kind:     KindFunction,
		Name:     "testFunc",
		File:     "/test/file.go",
		Line:     10,
		EndLine:  20,
		Body:     "func testFunc() {}",
		Language: "go",
	}

	id, err := database.UpsertNode(node)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	// Upsert again (should update)
	id2, err := database.UpsertNode(node)
	if err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	if id2 != id {
		t.Fatalf("expected same id, got %d vs %d", id, id2)
	}
}

func TestGetNodeByName(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert multiple nodes with same name
	database.UpsertNode(&Node{
		Kind: KindFunction, Name: "foo", File: "/a.go", Line: 1,
	})
	database.UpsertNode(&Node{
		Kind: KindFunction, Name: "foo", File: "/b.go", Line: 5,
	})
	database.UpsertNode(&Node{
		Kind: KindFunction, Name: "bar", File: "/a.go", Line: 10,
	})

	nodes, err := database.GetNodeByName("foo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}

	nodes, err = database.GetNodeByName("bar")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	nodes, err = database.GetNodeByName("nonexistent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(nodes))
	}
}

func TestGetNodeByFileLine(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.UpsertNode(&Node{
		Kind: KindFunction, Name: "foo", File: "/a.go", Line: 10, EndLine: 20,
	})
	database.UpsertNode(&Node{
		Kind: KindFunction, Name: "bar", File: "/a.go", Line: 30, EndLine: 40,
	})

	// Should find foo at line 15
	node, err := database.GetNodeByFileLine("/a.go", 15)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if node == nil || node.Name != "foo" {
		t.Fatalf("expected foo, got %v", node)
	}

	// Should find bar at line 35
	node, err = database.GetNodeByFileLine("/a.go", 35)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if node == nil || node.Name != "bar" {
		t.Fatalf("expected bar, got %v", node)
	}

	// Should return nil for non-existent file
	node, err = database.GetNodeByFileLine("/nonexistent.go", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if node != nil {
		t.Fatalf("expected nil, got %v", node)
	}
}

func TestUpsertEdge(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "caller", File: "/a.go", Line: 1,
	})
	id2, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "callee", File: "/b.go", Line: 1,
	})

	edge := &Edge{
		SourceID: id1,
		TargetID: id2,
		Kind:     EdgeCalls,
		File:     "/a.go",
		Line:     5,
	}

	edgeID, err := database.UpsertEdge(edge)
	if err != nil {
		t.Fatalf("upsert edge: %v", err)
	}
	if edgeID == 0 {
		t.Fatal("expected non-zero edge id")
	}

	// Upsert again (should update)
	edgeID2, err := database.UpsertEdge(edge)
	if err != nil {
		t.Fatalf("upsert edge2: %v", err)
	}
	if edgeID2 != edgeID {
		t.Fatalf("expected same edge id, got %d vs %d", edgeID, edgeID2)
	}
}

func TestGetCallers(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "caller", File: "/a.go", Line: 1,
	})
	id2, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "callee", File: "/b.go", Line: 1,
	})

	database.UpsertEdge(&Edge{
		SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 5,
	})

	callers, err := database.GetCallers(id2)
	if err != nil {
		t.Fatalf("get callers: %v", err)
	}
	if len(callers) != 1 {
		t.Fatalf("expected 1 caller, got %d", len(callers))
	}
	if callers[0].Name != "caller" {
		t.Fatalf("expected caller, got %s", callers[0].Name)
	}

	// No callers for id1
	callers, err = database.GetCallers(id1)
	if err != nil {
		t.Fatalf("get callers: %v", err)
	}
	if len(callers) != 0 {
		t.Fatalf("expected 0 callers, got %d", len(callers))
	}
}

func TestGetCallees(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "caller", File: "/a.go", Line: 1,
	})
	id2, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "callee", File: "/b.go", Line: 1,
	})

	database.UpsertEdge(&Edge{
		SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 5,
	})

	callees, err := database.GetCallees(id1)
	if err != nil {
		t.Fatalf("get callees: %v", err)
	}
	if len(callees) != 1 {
		t.Fatalf("expected 1 callee, got %d", len(callees))
	}
	if callees[0].Name != "callee" {
		t.Fatalf("expected callee, got %s", callees[0].Name)
	}

	// No callees for id2
	callees, err = database.GetCallees(id2)
	if err != nil {
		t.Fatalf("get callees: %v", err)
	}
	if len(callees) != 0 {
		t.Fatalf("expected 0 callees, got %d", len(callees))
	}
}

func TestFileNeedsReindex(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// New file should need reindex
	needs, err := database.FileNeedsReindex("/test.go", 100, 1000.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !needs {
		t.Fatal("new file should need reindex")
	}

	// Record file
	database.UpsertFile("/test.go", 100, 1000.0)

	// Same size/mtime should not need reindex
	needs, err = database.FileNeedsReindex("/test.go", 100, 1000.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if needs {
		t.Fatal("same file should not need reindex")
	}

	// Different size should need reindex
	needs, err = database.FileNeedsReindex("/test.go", 200, 1000.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !needs {
		t.Fatal("different size should need reindex")
	}

	// Different mtime should need reindex
	needs, err = database.FileNeedsReindex("/test.go", 100, 2000.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !needs {
		t.Fatal("different mtime should need reindex")
	}
}

func TestClearFile(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "foo", File: "/a.go", Line: 1,
	})
	id2, _ := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "bar", File: "/a.go", Line: 10,
	})
	database.UpsertEdge(&Edge{
		SourceID: id1, TargetID: id2, Kind: EdgeCalls, File: "/a.go", Line: 5,
	})
	database.UpsertFile("/a.go", 100, 1000.0)

	// Clear file
	if err := database.ClearFile("/a.go"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// Nodes should be gone
	nodes, _ := database.GetNodeByName("foo")
	if len(nodes) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(nodes))
	}

	// Edges should cascade-delete (S-17: FK CASCADE from nodes to edges).
	stats, err := database.GetStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.EdgeCount != 0 {
		t.Fatalf("expected 0 edges after ClearFile (CASCADE), got %d", stats.EdgeCount)
	}

	// File should be gone
	files, _ := database.ListFiles()
	for _, f := range files {
		if f == "/a.go" {
			t.Fatal("file should be removed")
		}
	}
}

func TestGetStats(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.UpsertNode(&Node{Kind: KindFunction, Name: "a", File: "/a.go", Line: 1})
	database.UpsertNode(&Node{Kind: KindFunction, Name: "b", File: "/b.go", Line: 1})
	database.UpsertNode(&Node{Kind: KindClass, Name: "C", File: "/c.go", Line: 1})
	database.UpsertFile("/a.go", 100, 1000.0)
	database.UpsertFile("/b.go", 200, 2000.0)

	stats, err := database.GetStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.NodeCount != 3 {
		t.Fatalf("expected 3 nodes, got %d", stats.NodeCount)
	}
	if stats.FileCount != 2 {
		t.Fatalf("expected 2 files, got %d", stats.FileCount)
	}
	if stats.KindCounts[KindFunction] != 2 {
		t.Fatalf("expected 2 functions, got %d", stats.KindCounts[KindFunction])
	}
	if stats.KindCounts[KindClass] != 1 {
		t.Fatalf("expected 1 class, got %d", stats.KindCounts[KindClass])
	}
}

func TestFindImporters(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Create file nodes
	fileA, _ := database.UpsertNode(&Node{Kind: KindFile, Name: "/a.go", File: "/a.go", Line: 0})
	fileB, _ := database.UpsertNode(&Node{Kind: KindFile, Name: "/b.go", File: "/b.go", Line: 0})

	// Create module node
	mod, _ := database.UpsertNode(&Node{Kind: "module", Name: "github.com/test/pkg", File: "github.com/test/pkg", Line: 0})

	// Create import edges
	database.UpsertEdge(&Edge{SourceID: fileA, TargetID: mod, Kind: EdgeImports, File: "/a.go", Line: 1})
	database.UpsertEdge(&Edge{SourceID: fileB, TargetID: mod, Kind: EdgeImports, File: "/b.go", Line: 1})

	// Find importers
	importers, err := database.FindImporters("github.com/test/pkg")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(importers) != 2 {
		t.Fatalf("expected 2 importers, got %d", len(importers))
	}
}

func TestNodeKinds(t *testing.T) {
	kinds := []string{
		KindFunction, KindClass, KindMethod, KindVariable,
		KindConstant, KindType, KindStruct, KindInterface, KindFile,
	}
	for _, k := range kinds {
		if k == "" {
			t.Fatal("empty kind")
		}
	}
}

func TestEdgeKinds(t *testing.T) {
	kinds := []string{
		EdgeCalls, EdgeImports, EdgeExtends, EdgeImplements, EdgeReferences,
	}
	for _, k := range kinds {
		if k == "" {
			t.Fatal("empty edge kind")
		}
	}
}

func TestFullTextSearch(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "UserService", File: "/svc.go", Line: 1,
		Body: "func UserService() {}", Language: "go",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "AuthHelper", File: "/auth.go", Line: 1,
		Body: "func AuthHelper() {}", Language: "go",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	nodes, err := database.FullTextSearch("UserService", 10)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("expected FTS hit for UserService")
	}
	if nodes[0].Name != "UserService" {
		t.Fatalf("got %s", nodes[0].Name)
	}

	// default limit path
	if _, err := database.FullTextSearch("AuthHelper", 0); err != nil {
		t.Fatalf("fts default limit: %v", err)
	}
}

func TestEscapeFTS5Query(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"foo", `"foo"`},
		{"foo bar", `"foo" "bar"`},
		{"AND", `"AND"`},
		{`"foo`, `"""foo"`},
		{":::", `":::"`},
		{"foo*", `"foo"*`},
		{"  spaced  out ", `"spaced" "out"`},
	}

	for _, tt := range tests {
		got := escapeFTS5Query(tt.in)
		if got != tt.want {
			t.Errorf("escapeFTS5Query(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
}

func TestFullTextSearchSpecialQueries(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "foo", File: "/a.go", Line: 1,
		Body: "func foo()", Language: "go",
	}); err != nil {
		t.Fatal(err)
	}

	// These used to raise FTS5 syntax errors.
	for _, q := range []string{"foo", "AND", "\"foo", ":::", "foo*"} {
		nodes, err := database.FullTextSearch(q, 10)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		_ = nodes
	}
	nodes, err := database.FullTextSearch("foo", 10)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("foo: err=%v n=%d", err, len(nodes))
	}
}

func TestFTSBackfillOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	codegraph := filepath.Join(dir, ".codegraph")
	if err := os.MkdirAll(codegraph, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(codegraph, "codegraph.db")

	// Simulate a pre-FTS database: nodes only, no FTS table.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
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
		INSERT INTO nodes(kind, name, file, line, body, language)
		VALUES ('function', 'LegacyFn', '/l.go', 1, 'body', 'go');
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open upgrade: %v", err)
	}
	defer database.Close()

	nodes, err := database.FullTextSearch("LegacyFn", 10)
	if err != nil {
		t.Fatalf("fts after upgrade: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "LegacyFn" {
		t.Fatalf("expected LegacyFn backfilled, got %+v", nodes)
	}
}

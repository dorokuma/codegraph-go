package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// M-12: underscore in package name must not be treated as LIKE wildcard.
	modU, _ := database.UpsertNode(&Node{Kind: "module", Name: "github.com/test/pkg_util", File: "github.com/test/pkg_util", Line: 0})
	subPkg, _ := database.UpsertNode(&Node{Kind: "module", Name: "github.com/test/pkg_util/sub", File: "github.com/test/pkg_util/sub", Line: 0})
	fileC, _ := database.UpsertNode(&Node{Kind: KindFile, Name: "/c.go", File: "/c.go", Line: 0})
	database.UpsertEdge(&Edge{SourceID: fileC, TargetID: modU, Kind: EdgeImports, File: "/c.go", Line: 1})
	database.UpsertEdge(&Edge{SourceID: fileC, TargetID: subPkg, Kind: EdgeImports, File: "/c.go", Line: 2})
	// Also create a distractor package that differs by one char (would match if _ were wildcard)
	modX, _ := database.UpsertNode(&Node{Kind: "module", Name: "github.com/test/pkgXutil", File: "github.com/test/pkgXutil", Line: 0})
	fileD, _ := database.UpsertNode(&Node{Kind: KindFile, Name: "/d.go", File: "/d.go", Line: 0})
	database.UpsertEdge(&Edge{SourceID: fileD, TargetID: modX, Kind: EdgeImports, File: "/d.go", Line: 1})

	importers2, err := database.FindImporters("github.com/test/pkg_util")
	if err != nil {
		t.Fatalf("find underscore pkg: %v", err)
	}
	if len(importers2) != 1 {
		t.Fatalf("expected 1 importer (c.go via DISTINCT), got %d: %v", len(importers2), importers2)
	}
	// Verify /d.go (pkgXutil) is NOT included — _ escaped, not LIKE wildcard.
	for _, f := range importers2 {
		if f == "/d.go" {
			t.Fatalf("pkgXutil incorrectly matched as importer of pkg_util — LIKE _ escape failed")
		}
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
	// S-19: snake_case names with underscores must be findable via FTS
	// (tokenize tokenchars='_' prevents splitting on underscore).
	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "foo_bar", File: "/snake.go", Line: 1,
		Body: "func foo_bar() {}", Language: "go",
	}); err != nil {
		t.Fatalf("upsert foo_bar: %v", err)
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

	// S-19: verify snake_case name is not split on underscore
	nodes, err = database.FullTextSearch("foo_bar", 10)
	if err != nil {
		t.Fatalf("fts foo_bar: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "foo_bar" {
		t.Fatalf("expected foo_bar via FTS, got %d nodes", len(nodes))
	}

	// language=go must not match a query of "go" when name/body lack that token.
	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "Alpha", File: "/alpha.go", Line: 1,
		Body: "func Alpha() {}", Language: "go",
	}); err != nil {
		t.Fatalf("upsert Alpha: %v", err)
	}
	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "Runner", File: "/runner.py", Line: 1,
		Body: "def Runner(): go_fast()", Language: "python",
	}); err != nil {
		t.Fatalf("upsert Runner: %v", err)
	}
	nodes, err = database.FullTextSearch("go", 20)
	if err != nil {
		t.Fatalf("fts go: %v", err)
	}
	for _, n := range nodes {
		if n.Name == "Alpha" || n.Name == "UserService" || n.Name == "AuthHelper" || n.Name == "foo_bar" {
			t.Fatalf("query go matched %s via language column", n.Name)
		}
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

func TestInsertFact(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	f := &Fact{
		TargetFile:   "alpha.go",
		TargetSymbol: "Alpha",
		TargetLine:   10,
		Content:      "Alpha is the entry point for parsing",
		ContentHash:  "abc123",
		Author:       "agent",
		Status:       "active",
	}
	id, err := database.InsertFact(f)
	if err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	// Duplicate hash on the same target should fail (UNIQUE constraint)
	f2 := *f
	_, err = database.InsertFact(&f2)
	if err == nil {
		t.Fatal("expected error on duplicate hash+target")
	}
	// Same text on a different symbol is allowed.
	f3 := *f
	f3.TargetFile = "beta.go"
	f3.TargetSymbol = "Beta"
	if _, err = database.InsertFact(&f3); err != nil {
		t.Fatalf("same hash different target: %v", err)
	}
}

func TestGetFactByHash(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	f := &Fact{
		TargetFile:   "beta.go",
		TargetSymbol: "Beta",
		Content:      "Beta processes user input",
		ContentHash:  "def456",
		Status:       "active",
	}
	id, err := database.InsertFact(f)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := database.GetFactByHash("def456")
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got == nil {
		t.Fatal("expected fact, got nil")
	}
	if got.ID != id {
		t.Fatalf("expected id %d, got %d", id, got.ID)
	}
	if got.Content != "Beta processes user input" {
		t.Fatalf("content mismatch: %q", got.Content)
	}
	if got.TargetFile != "beta.go" {
		t.Fatalf("file mismatch: %q", got.TargetFile)
	}

	// Non-existent hash
	nilFact, err := database.GetFactByHash("nonexistent")
	if err != nil {
		t.Fatalf("get nonexistent: %v", err)
	}
	if nilFact != nil {
		t.Fatal("expected nil for nonexistent hash")
	}
}

func TestGetFactsByTarget(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.InsertFact(&Fact{
		TargetFile: "gamma.go", TargetSymbol: "Gamma", Content: "fact1", ContentHash: "h1", Status: "active",
	})
	database.InsertFact(&Fact{
		TargetFile: "gamma.go", TargetSymbol: "Gamma", Content: "fact2", ContentHash: "h2", Status: "active",
	})
	database.InsertFact(&Fact{
		TargetFile: "gamma.go", TargetSymbol: "Delta", Content: "fact3", ContentHash: "h3", Status: "active",
	})

	// Filter by file only
	facts, err := database.GetFactsByTarget("gamma.go", "")
	if err != nil {
		t.Fatalf("get by target: %v", err)
	}
	if len(facts) != 3 {
		t.Fatalf("expected 3 facts, got %d", len(facts))
	}

	// Filter by file + symbol
	facts, err = database.GetFactsByTarget("gamma.go", "Gamma")
	if err != nil {
		t.Fatalf("get by target+symbol: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts for Gamma, got %d", len(facts))
	}

	// No match
	facts, err = database.GetFactsByTarget("nonexistent.go", "")
	if err != nil {
		t.Fatalf("get nonexistent: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts, got %d", len(facts))
	}
}

// TestGetFactsByTargetLimited: the read must be capped (no unbounded memory
// for targets with huge fact piles) and truncation must be explicit.
func TestGetFactsByTargetLimited(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	old := maxFactsByTarget
	maxFactsByTarget = 3
	defer func() { maxFactsByTarget = old }()

	for i := 0; i < 10; i++ {
		if _, err := database.InsertFact(&Fact{
			TargetFile: "pile.go", TargetSymbol: "Pile",
			Content: fmt.Sprintf("fact%d", i), ContentHash: fmt.Sprintf("h%d", i),
			Status: "active",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// created_at has second resolution: all inserts above share one second,
	// which makes ORDER BY created_at a no-op tiebreaker. Give each row a
	// distinct timestamp so the "newest first" ordering is actually tested.
	if _, err := database.conn.Exec("UPDATE facts SET created_at = id"); err != nil {
		t.Fatal(err)
	}

	// Explicit-limit variant reports the truncation flag.
	facts, truncated, err := database.GetFactsByTargetLimited("pile.go", "Pile", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 4 || !truncated {
		t.Fatalf("limited: got %d facts, truncated=%v; want 4 and true", len(facts), truncated)
	}
	if facts[0].Content != "fact9" {
		t.Fatalf("newest fact must come first, got %q", facts[0].Content)
	}

	// Non-truncated read.
	facts, truncated, err = database.GetFactsByTargetLimited("pile.go", "Pile", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 10 || truncated {
		t.Fatalf("untruncated: got %d facts, truncated=%v; want 10 and false", len(facts), truncated)
	}

	// GetFactsByTarget delegates to the default cap (logs, still bounded).
	facts, err = database.GetFactsByTarget("pile.go", "Pile")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != maxFactsByTarget {
		t.Fatalf("GetFactsByTarget must cap at maxFactsByTarget=%d, got %d", maxFactsByTarget, len(facts))
	}
}

func TestSearchFacts(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.InsertFact(&Fact{
		TargetFile: "a.go", TargetSymbol: "Foo", Content: "Foo is a handler for /foo route", ContentHash: "h1", Status: "active",
	})
	database.InsertFact(&Fact{
		TargetFile: "a.go", TargetSymbol: "Bar", Content: "Bar validates the request body", ContentHash: "h2", Status: "active",
	})
	database.InsertFact(&Fact{
		TargetFile: "b.go", TargetSymbol: "Foo", Content: "Foo helper does logging", ContentHash: "h3", Status: "superseded",
	})

	// Search content substring (case-insensitive LIKE)
	facts, err := database.SearchFacts("handler", "", "", "", 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact matching 'handler', got %d", len(facts))
	}

	// Filter by file
	facts, err = database.SearchFacts("", "a.go", "", "", 20)
	if err != nil {
		t.Fatalf("search by file: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts for a.go, got %d", len(facts))
	}

	// Filter by symbol
	facts, err = database.SearchFacts("", "", "Foo", "", 20)
	if err != nil {
		t.Fatalf("search by symbol: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts for symbol Foo, got %d", len(facts))
	}

	// Filter by status
	facts, err = database.SearchFacts("", "", "", "active", 20)
	if err != nil {
		t.Fatalf("search by status: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 active facts, got %d", len(facts))
	}

	// status=all returns everything
	facts, err = database.SearchFacts("", "", "", "all", 20)
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if len(facts) != 3 {
		t.Fatalf("expected 3 facts for 'all', got %d", len(facts))
	}

	// Default max
	facts, err = database.SearchFacts("", "", "", "", 0)
	if err != nil {
		t.Fatalf("search default max: %v", err)
	}
	if len(facts) > 20 {
		t.Fatalf("expected at most 20 facts, got %d", len(facts))
	}
}

func TestSupersedeFact(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.InsertFact(&Fact{
		TargetFile: "a.go", TargetSymbol: "Foo", Content: "old claim", ContentHash: "h1", Status: "active",
	})
	id2, _ := database.InsertFact(&Fact{
		TargetFile: "a.go", TargetSymbol: "Foo", Content: "corrected claim", ContentHash: "h2", Status: "active",
	})

	// Supersede
	if err := database.SupersedeFact(id1, id2); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	// old should be superseded
	f1, err := database.GetFactByHash("h1")
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if f1.Status != "superseded" {
		t.Fatalf("expected 'superseded', got %q", f1.Status)
	}
	if f1.SupersededBy != id2 {
		t.Fatalf("expected superseded_by=%d, got %d", id2, f1.SupersededBy)
	}

	// Re-superseding should fail (already inactive)
	if err := database.SupersedeFact(id1, id2); err == nil {
		t.Fatal("expected error on superseding inactive fact")
	}
}

func TestInsertFactSuperseding(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, err := database.InsertFactSuperseding(&Fact{
		TargetFile: "a.go", TargetSymbol: "Foo", Content: "old", ContentHash: "h1", Status: "active",
	}, 0)
	if err != nil || id1 == 0 {
		t.Fatalf("insert only: id=%d err=%v", id1, err)
	}

	id2, err := database.InsertFactSuperseding(&Fact{
		TargetFile: "a.go", TargetSymbol: "Foo", Content: "new", ContentHash: "h2", Status: "active",
	}, id1)
	if err != nil {
		t.Fatalf("insert+supersede: %v", err)
	}
	old, err := database.GetFactByHash("h1")
	if err != nil || old == nil {
		t.Fatalf("get old: %v", err)
	}
	if old.Status != "superseded" || old.SupersededBy != id2 {
		t.Fatalf("old status=%q superseded_by=%d want superseded/%d", old.Status, old.SupersededBy, id2)
	}

	// Failed supersede must roll back the insert (no orphan row).
	if _, err := database.InsertFactSuperseding(&Fact{
		TargetFile: "a.go", Content: "orphan", ContentHash: "h3", Status: "active",
	}, id1); err == nil {
		t.Fatal("expected error superseding inactive fact")
	}
	got, err := database.GetFactByHash("h3")
	if err != nil {
		t.Fatalf("get orphan: %v", err)
	}
	if got != nil {
		t.Fatalf("orphan fact inserted despite failed supersede: %+v", got)
	}
}

func TestRetractFact(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id, _ := database.InsertFact(&Fact{
		TargetFile: "a.go", Content: "wrong", ContentHash: "h1", Status: "active",
	})

	if err := database.RetractFact(id); err != nil {
		t.Fatalf("retract: %v", err)
	}

	f, err := database.GetFactByHash("h1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if f.Status != "retracted" {
		t.Fatalf("expected 'retracted', got %q", f.Status)
	}

	// Double retract should fail
	if err := database.RetractFact(id); err == nil {
		t.Fatal("expected error on retracting inactive fact")
	}
}

func TestFactsSurviveWipeIndex(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert a fact
	id, err := database.InsertFact(&Fact{
		TargetFile:   "survivor.go",
		TargetSymbol: "Survivor",
		Content:      "this fact must survive WipeIndex",
		ContentHash:  "survive-hash",
		Status:       "active",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Also insert a node (will be wiped)
	database.UpsertNode(&Node{
		Kind: KindFunction, Name: "willBeWiped", File: "/gone.go", Line: 1,
	})

	// WipeIndex should only delete unresolved_refs/edges/nodes/files
	if err := database.WipeIndex(); err != nil {
		t.Fatalf("WipeIndex: %v", err)
	}

	// Fact should still exist
	f, err := database.GetFactByHash("survive-hash")
	if err != nil {
		t.Fatalf("get fact after wipe: %v", err)
	}
	if f == nil {
		t.Fatal("fact disappeared after WipeIndex")
	}
	if f.ID != id {
		t.Fatalf("expected id %d, got %d", id, f.ID)
	}
	if f.Status != "active" {
		t.Fatalf("expected active, got %q", f.Status)
	}

	// Nodes table should be empty
	stats, err := database.GetStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.NodeCount != 0 {
		t.Fatalf("expected 0 nodes after wipe, got %d", stats.NodeCount)
	}
}

func TestFactsRelPath(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert with absolute path — stored as-is; RelPath conversion happens at the tool layer
	id, err := database.InsertFact(&Fact{
		TargetFile:   "/home/user/project/src/main.go",
		TargetSymbol: "Main",
		Content:      "Main starts the server",
		ContentHash:  "abs-path-hash",
		Status:       "active",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	f, err := database.GetFactByHash("abs-path-hash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if f.TargetFile != "/home/user/project/src/main.go" {
		t.Fatalf("expected absolute path, got %q", f.TargetFile)
	}
	_ = id
}

func TestSupersedeChain(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.InsertFact(&Fact{
		TargetFile: "x.go", TargetSymbol: "X", Content: "v1", ContentHash: "v1-hash", Status: "active",
	})
	id2, _ := database.InsertFact(&Fact{
		TargetFile: "x.go", TargetSymbol: "X", Content: "v2", ContentHash: "v2-hash", Status: "active",
	})
	id3, _ := database.InsertFact(&Fact{
		TargetFile: "x.go", TargetSymbol: "X", Content: "v3", ContentHash: "v3-hash", Status: "active",
	})

	// v1 → v2 → v3
	if err := database.SupersedeFact(id1, id2); err != nil {
		t.Fatalf("v1→v2: %v", err)
	}
	if err := database.SupersedeFact(id2, id3); err != nil {
		t.Fatalf("v2→v3: %v", err)
	}

	f1, _ := database.GetFactByHash("v1-hash")
	f2, _ := database.GetFactByHash("v2-hash")
	f3, _ := database.GetFactByHash("v3-hash")

	if f1.Status != "superseded" || f1.SupersededBy != id2 {
		t.Fatalf("v1 wrong: status=%q superseded_by=%d", f1.Status, f1.SupersededBy)
	}
	if f2.Status != "superseded" || f2.SupersededBy != id3 {
		t.Fatalf("v2 wrong: status=%q superseded_by=%d", f2.Status, f2.SupersededBy)
	}
	if f3.Status != "active" || f3.SupersededBy != 0 {
		t.Fatalf("v3 wrong: status=%q superseded_by=%d", f3.Status, f3.SupersededBy)
	}
}

func TestListUnresolvedRefsByNames(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	fromID, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "holder", File: "/h.go", Line: 1, Language: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(name, tail, status string) int64 {
		t.Helper()
		id, err := database.InsertUnresolvedRef(&UnresolvedRef{
			FromNode:      fromID,
			ReferenceName: name,
			ReferenceKind: EdgeCalls,
			Line:          2,
			FilePath:      "/a.go",
			Language:      "go",
			Status:        status,
			NameTail:      tail,
		})
		if err != nil {
			t.Fatalf("insert ref %s: %v", name, err)
		}
		return id
	}
	rName := insert("Foo", "Foo", "failed")           // matches by reference_name
	rTail := insert("pkg.Foo", "Foo", "pending")      // matches by name_tail
	rBoth := insert("Bar", "Bar", "failed")           // matches both branches (dedupe)
	rNoMatch := insert("Baz", "Baz", "pending")       // no match
	rWrongStatus := insert("Quux", "Quux", "pending") // name matches only in failed query

	got, err := database.ListUnresolvedRefsByNames([]string{"Foo", "Bar", "Nope"}, []string{"pending", "failed"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{rName: true, rTail: true, rBoth: true}
	if len(got) != len(want) {
		t.Fatalf("want %d rows, got %d: %+v", len(want), len(got), got)
	}
	for _, r := range got {
		if !want[r.ID] {
			t.Fatalf("unexpected row id %d (%s/%s)", r.ID, r.ReferenceName, r.Status)
		}
	}
	if r := got[0]; r.ID != rName && r.ID != rTail && r.ID != rBoth {
		t.Fatalf("expected a matched ref, got %+v", got[0])
	}
	_ = rNoMatch
	_ = rWrongStatus

	// Status filter: only failed.
	got, err = database.ListUnresolvedRefsByNames([]string{"Foo", "Bar"}, []string{"failed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 { // rName + rBoth; rTail is pending
		t.Fatalf("failed-only: want 2 rows, got %d: %+v", len(got), got)
	}
	// Status filter excludes: rWrongStatus is pending, so a failed query finds nothing.
	got, err = database.ListUnresolvedRefsByNames([]string{"Quux"}, []string{"failed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Quux is pending: failed query must return 0, got %+v", got)
	}
	// No status filter returns both statuses.
	got, err = database.ListUnresolvedRefsByNames([]string{"Quux"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != rWrongStatus {
		t.Fatalf("no-status query: want Quux row, got %+v", got)
	}
	// Empty names → nil without querying.
	got, err = database.ListUnresolvedRefsByNames(nil, nil)
	if err != nil || got != nil {
		t.Fatalf("empty names: got %+v err %v", got, err)
	}
	// Chunking: >400 names must not exceed SQLite's variable limit and must
	// still find matches in later chunks.
	names := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		names = append(names, fmt.Sprintf("no_such_%d", i))
	}
	names[999] = "Bar"
	got, err = database.ListUnresolvedRefsByNames(names, []string{"failed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != rBoth {
		t.Fatalf("chunked query: want rBoth, got %+v", got)
	}
}

func TestReplaceSynthesizedEdges(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	a, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "a", File: "/a.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "b", File: "/b.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	countSynthesized := func() int {
		var n int
		if err := database.conn.QueryRow(`
			SELECT COUNT(*) FROM edges
			WHERE provenance = 'heuristic' AND metadata LIKE '%synthesizedBy%'
		`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	countTotal := func() int {
		var n int
		if err := database.conn.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Seed: one synthesized edge (line 0) + one non-synthesized exact edge (line 10).
	if _, err := database.UpsertEdge(&Edge{
		SourceID: a, TargetID: b, Kind: EdgeCalls, File: "/a.go",
		Line: 0, Provenance: "heuristic", Metadata: `{"synthesizedBy":"old"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertEdge(&Edge{
		SourceID: a, TargetID: b, Kind: EdgeCalls, File: "/a.go",
		Line: 10, Provenance: "exact", Metadata: "",
	}); err != nil {
		t.Fatal(err)
	}

	// Success: old synthesized edges replaced, non-synthesized untouched.
	if err := database.ReplaceSynthesizedEdges([]Edge{{
		SourceID: a, TargetID: b, Kind: EdgeCalls, File: "/a.go",
		Line: 0, Provenance: "heuristic", Metadata: `{"synthesizedBy":"new"}`,
	}}); err != nil {
		t.Fatal(err)
	}
	if n := countSynthesized(); n != 1 {
		t.Fatalf("want 1 synthesized edge after replace, got %d", n)
	}
	if n := countTotal(); n != 2 {
		t.Fatalf("want 2 edges total (synth + exact), got %d", n)
	}
	var meta string
	if err := database.conn.QueryRow(`
		SELECT metadata FROM edges WHERE source_id = ? AND target_id = ? AND kind = ? AND line = 0
	`, a, b, EdgeCalls).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != `{"synthesizedBy":"new"}` {
		t.Fatalf("expected new metadata, got %q", meta)
	}

	// Failure injection: an edge whose source violates the nodes FK. The whole
	// batch must roll back and the old synthesized edge must survive.
	if err := database.ReplaceSynthesizedEdges([]Edge{{
		SourceID: 999999, TargetID: b, Kind: EdgeCalls, File: "/a.go",
		Line: 0, Provenance: "heuristic", Metadata: `{"synthesizedBy":"bad"}`,
	}}); err == nil {
		t.Fatal("expected FK violation error")
	}
	if n := countSynthesized(); n != 1 {
		t.Fatalf("old synthesized edges must survive a failed replace, got %d", n)
	}
	if n := countTotal(); n != 2 {
		t.Fatalf("total edges must survive a failed replace, got %d", n)
	}
	if err := database.conn.QueryRow(`
		SELECT metadata FROM edges WHERE source_id = ? AND target_id = ? AND kind = ? AND line = 0
	`, a, b, EdgeCalls).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != `{"synthesizedBy":"new"}` {
		t.Fatalf("metadata must stay at the pre-failure value, got %q", meta)
	}

	// Empty edge set clears synthesized edges (idempotent re-run) but keeps exact.
	if err := database.ReplaceSynthesizedEdges(nil); err != nil {
		t.Fatal(err)
	}
	if n := countSynthesized(); n != 0 {
		t.Fatalf("empty replace must clear synthesized edges, got %d", n)
	}
	if n := countTotal(); n != 1 {
		t.Fatalf("exact edge must survive empty replace, got %d", n)
	}
}

func TestReplaceFileIndexModuleNodesAtomic(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	fileNode := Node{Kind: KindFile, Name: "a.go", File: "a.go", Line: 0, Language: "go"}
	fr := &FileRecord{Path: "a.go", Size: 10, Mtime: 1, ContentHash: "h", Language: "go", NodeCount: 1}

	// Success: the module node is created inside the transaction and the
	// import edge (placeholder -2 = moduleNodes[0] since len(nodes)==1)
	// resolves to its real id.
	ids, err := database.ReplaceFileIndex("a.go", []Node{fileNode}, []Edge{{
		SourceID: -1, TargetID: -2, Kind: EdgeImports, File: "a.go", Line: 1, Provenance: "exact",
	}}, nil, fr, Node{Kind: "module", Name: "mod/ok", File: "mod/ok", Line: 0, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	mods, err := database.GetNodeByName("mod/ok")
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 1 || mods[0].Kind != "module" {
		t.Fatalf("expected one module node, got %+v", mods)
	}
	e, err := database.GetEdgeByEndpoints(ids[0], mods[0].ID, EdgeImports)
	if err != nil {
		t.Fatal(err)
	}
	if e == nil {
		t.Fatal("expected imports edge file → module")
	}

	// Failure: a dangling positive target id violates the edges FK mid-batch.
	// The module node must roll back with the transaction — no orphan left.
	_, err = database.ReplaceFileIndex("b.go", []Node{{Kind: KindFile, Name: "b.go", File: "b.go", Line: 0, Language: "go"}}, []Edge{{
		SourceID: -1, TargetID: 999999, Kind: EdgeImports, File: "b.go", Line: 1, Provenance: "exact",
	}}, nil, fr, Node{Kind: "module", Name: "mod/bad", File: "mod/bad", Line: 0, Language: "go"})
	if err == nil {
		t.Fatal("expected FK violation error")
	}
	bad, err := database.GetNodeByName("mod/bad")
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("module node must not survive a failed batch, got %+v", bad)
	}
	// The successful batch's module node and edge are untouched.
	mods, err = database.GetNodeByName("mod/ok")
	if err != nil || len(mods) != 1 {
		t.Fatalf("successful module node must survive, got %+v err=%v", mods, err)
	}

	// S3/F5: a conflicting module node (same conflict key) must refresh its
	// language like the old UpsertNode DO UPDATE, not silently keep the
	// stale value.
	_, err = database.ReplaceFileIndex("c.go", []Node{{Kind: KindFile, Name: "c.go", File: "c.go", Line: 0, Language: "go"}}, nil, nil, fr,
		Node{Kind: "module", Name: "mod/ok", File: "mod/ok", Line: 0, Language: "rust"})
	if err != nil {
		t.Fatal(err)
	}
	mods, err = database.GetNodeByName("mod/ok")
	if err != nil || len(mods) != 1 {
		t.Fatalf("conflicting module node must still resolve, got %+v err=%v", mods, err)
	}
	if mods[0].Language != "rust" {
		t.Fatalf("conflicting module node must refresh language, got %+v", mods)
	}
}

// TestReplaceFileIndexPlaceholderOutOfRange: a negative placeholder id that
// falls outside BOTH the batch-node range and the module-node range used to
// index out of bounds and panic. It must now return a diagnostic error and
// roll the transaction back (nothing written).
func TestReplaceFileIndexPlaceholderOutOfRange(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	fileNode := Node{Kind: KindFile, Name: "oob.go", File: "oob.go", Line: 0, Language: "go"}

	// Panic path A: node-placeholder range overrun with NO module nodes —
	// with len(nodes)==1, any placeholder <= -3 used to panic on
	// moduleIDs[idx-len(ids)].
	_, err := database.ReplaceFileIndex("oob.go", []Node{fileNode}, []Edge{{
		SourceID: -3, TargetID: -2, Kind: EdgeCalls, File: "oob.go", Line: 1, Provenance: "exact",
	}}, nil, nil)
	if err == nil {
		t.Fatal("expected error for out-of-range node placeholder")
	}
	if !strings.Contains(err.Error(), "placeholder") || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected a diagnostic placeholder error, got: %v", err)
	}
	// Transaction must have rolled back: no nodes/edges for oob.go.
	if nodes, _ := database.GetNodesByFile("oob.go"); len(nodes) != 0 {
		t.Fatalf("failed batch must roll back, got %d nodes for oob.go", len(nodes))
	}

	// Panic path B: module-placeholder range overrun — with len(nodes)==1
	// and 1 module node, placeholder -4 (idx=3, mIdx=2) used to panic.
	_, err = database.ReplaceFileIndex("oob.go", []Node{fileNode}, []Edge{{
		SourceID: -1, TargetID: -4, Kind: EdgeImports, File: "oob.go", Line: 1, Provenance: "exact",
	}}, nil, nil, Node{Kind: "module", Name: "mod/oob", File: "mod/oob", Line: 0, Language: "go"})
	if err == nil {
		t.Fatal("expected error for out-of-range module placeholder")
	}
	if !strings.Contains(err.Error(), "placeholder") || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected a diagnostic placeholder error, got: %v", err)
	}
	// Rollback: neither the file node nor the module node may survive.
	if nodes, _ := database.GetNodesByFile("oob.go"); len(nodes) != 0 {
		t.Fatalf("failed batch must roll back, got %d nodes for oob.go", len(nodes))
	}
	if mods, _ := database.GetNodeByName("mod/oob"); len(mods) != 0 {
		t.Fatalf("module node must roll back with the failed batch, got %+v", mods)
	}

	// Unresolved-ref from_node placeholder out of range is caught too.
	_, err = database.ReplaceFileIndex("oob.go", []Node{fileNode}, nil, []UnresolvedRef{{
		FromNode: -7, ReferenceName: "x", ReferenceKind: EdgeCalls, Line: 1,
		FilePath: "oob.go", Language: "go", Status: "pending", NameTail: "x",
	}}, nil)
	if err == nil {
		t.Fatal("expected error for out-of-range unresolved_ref placeholder")
	}
	if !strings.Contains(err.Error(), "placeholder") || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected a diagnostic placeholder error, got: %v", err)
	}
	if refs, _ := database.ListUnresolvedRefs("oob.go", ""); len(refs) != 0 {
		t.Fatalf("failed batch must roll back unresolved_refs, got %+v", refs)
	}

	// Control: an in-range placeholder batch still succeeds after the
	// failures above.
	ids, cerr := database.ReplaceFileIndex("ok.go", []Node{{Kind: KindFile, Name: "ok.go", File: "ok.go", Line: 0, Language: "go"}}, []Edge{{
		SourceID: -1, TargetID: -2, Kind: EdgeImports, File: "ok.go", Line: 1, Provenance: "exact",
	}}, nil, nil, Node{Kind: "module", Name: "mod/ok2", File: "mod/ok2", Line: 0, Language: "go"})
	if cerr != nil {
		t.Fatalf("in-range placeholder batch must succeed, got %v", cerr)
	}
	if len(ids) != 1 || ids[0] == 0 {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

// TestSearchFactsLiteralWildcards: % and _ in the search query must be
// matched literally (ESCAPE '\'), never expanded as LIKE wildcards — a
// search for "50%" must not match every row containing "50", and "_" must
// not match every non-empty row.
func TestSearchFactsLiteralWildcards(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.InsertFact(&Fact{TargetFile: "a.go", Content: "50% done with the refactor", ContentHash: "h1", Status: "active"})
	database.InsertFact(&Fact{TargetFile: "b.go", Content: "rename_to_new_name", ContentHash: "h2", Status: "active"})
	database.InsertFact(&Fact{TargetFile: "c.go", Content: "plain text content", ContentHash: "h3", Status: "active"})

	// Literal %: only the row that actually contains '%' matches.
	facts, err := database.SearchFacts("50%", "", "", "all", 20)
	if err != nil {
		t.Fatalf("search 50%%: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "50% done with the refactor" {
		t.Fatalf("search '50%%' = %+v, want only the literal-%% row", facts)
	}

	// A bare '%' must match only content containing a literal '%'.
	facts, err = database.SearchFacts("%", "", "", "all", 20)
	if err != nil {
		t.Fatalf("search %%: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "50% done with the refactor" {
		t.Fatalf("search '%%' = %+v, want only the literal-%% row", facts)
	}

	// Literal _: matches only the underscore row — without escaping, '_'
	// matches ANY single character and would return all three rows.
	facts, err = database.SearchFacts("rename_to_new_name", "", "", "all", 20)
	if err != nil {
		t.Fatalf("search underscore: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "rename_to_new_name" {
		t.Fatalf("search 'rename_to_new_name' = %+v, want only the literal-underscore row", facts)
	}
	facts, err = database.SearchFacts("_", "", "", "all", 20)
	if err != nil {
		t.Fatalf("search bare underscore: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "rename_to_new_name" {
		t.Fatalf("search '_' = %+v, want only the literal-underscore row", facts)
	}

	// No wildcard cross-matching: '50_done' must NOT match '50% done'
	// (unescaped '_' would match the '%' character).
	facts, err = database.SearchFacts("50_done", "", "", "all", 20)
	if err != nil {
		t.Fatalf("search 50_done: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("search '50_done' = %+v, want no wildcard cross-match", facts)
	}

	// Control: a normal substring still works.
	facts, err = database.SearchFacts("plain text", "", "", "all", 20)
	if err != nil {
		t.Fatalf("search plain: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "plain text content" {
		t.Fatalf("control search = %+v", facts)
	}
}

// TestSupersedeFactValidatesNewID: a supersede must never point at a missing
// or non-active fact, and failed attempts must not mutate anything.
func TestSupersedeFactValidatesNewID(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.InsertFact(&Fact{TargetFile: "a.go", Content: "old", ContentHash: "h1", Status: "active"})
	id2, _ := database.InsertFact(&Fact{TargetFile: "a.go", Content: "new", ContentHash: "h2", Status: "active"})
	dead, _ := database.InsertFact(&Fact{TargetFile: "a.go", Content: "dead", ContentHash: "h3", Status: "active"})
	if err := database.RetractFact(dead); err != nil {
		t.Fatal(err)
	}

	// newID missing
	if err := database.SupersedeFact(id1, 99999); err == nil {
		t.Fatal("expected error for missing newID")
	}
	// newID exists but is not active
	if err := database.SupersedeFact(id1, dead); err == nil {
		t.Fatal("expected error for non-active newID")
	}
	// failed attempts must not have mutated the old fact
	f1, err := database.GetFactByHash("h1")
	if err != nil {
		t.Fatal(err)
	}
	if f1.Status != "active" || f1.SupersededBy != 0 {
		t.Fatalf("failed supersede mutated old fact: status=%q superseded_by=%d", f1.Status, f1.SupersededBy)
	}
	// a valid supersede still works
	if err := database.SupersedeFact(id1, id2); err != nil {
		t.Fatalf("valid supersede: %v", err)
	}
}

// TestFullTextSearchRefsSkipsBody: the lightweight FTS variant must return
// file:line refs without loading bodies, while the full variant keeps them.
func TestFullTextSearchRefsSkipsBody(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	body := strings.Repeat("func UserService() { return 1 }\n", 200)
	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "UserService", File: "/svc.go", Line: 10, EndLine: 20,
		Body: body, Language: "go",
	}); err != nil {
		t.Fatal(err)
	}

	refs, err := database.FullTextSearchRefs("UserService", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if refs[0].File != "/svc.go" || refs[0].Line != 10 {
		t.Fatalf("ref = %s:%d, want /svc.go:10", refs[0].File, refs[0].Line)
	}
	if refs[0].Body != "" {
		t.Fatalf("lightweight FTS must not load body (got %d chars)", len(refs[0].Body))
	}
	// Context variant behaves identically.
	refs, err = database.FullTextSearchRefsContext(context.Background(), "UserService", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Body != "" {
		t.Fatalf("context variant: got %d refs, body=%d chars", len(refs), len(refs[0].Body))
	}

	// Full FTS still returns the body.
	full, err := database.FullTextSearch("UserService", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 1 || full[0].Body != body {
		t.Fatalf("full FTS must keep body (got %d chars)", len(full[0].Body))
	}
}

// TestListFilesInDirDirectChildrenOnly: only files directly in dir are
// returned — nested paths are excluded (in SQL, so the LIMIT counts direct
// children, not the whole subtree).
func TestListFilesInDirDirectChildrenOnly(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	for _, p := range []string{"root.go", "pkg/a.go", "pkg/b.go", "pkg/sub/c.go", "pkg2/x.go"} {
		if err := database.UpsertFile(p, 100, 1000.0); err != nil {
			t.Fatal(err)
		}
	}

	got, err := database.ListFilesInDir("pkg")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pkg: got %v, want [pkg/a.go pkg/b.go]", got)
	}
	for _, f := range got {
		if f == "pkg/sub/c.go" || f == "pkg2/x.go" || f == "root.go" {
			t.Fatalf("unexpected file in dir listing: %v", got)
		}
	}

	// Root listing keeps returning only direct children.
	got, err = database.ListFilesInDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "root.go" {
		t.Fatalf("root: got %v, want [root.go]", got)
	}
}

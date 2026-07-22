package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func seedGraph(t *testing.T) (*db.DB, string, func()) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// foo calls bar; main calls foo
	fooFile := filepath.Join(dir, "foo.go")
	barFile := filepath.Join(dir, "bar.go")
	mainFile := filepath.Join(dir, "main.go")

	barID, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "bar", File: barFile, Line: 1, Body: "func bar() {}", Language: "go"})
	fooID, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "foo", File: fooFile, Line: 1, Body: "func foo() { bar() }", Language: "go"})
	mainID, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "main", File: mainFile, Line: 1, Body: "func main() { foo() }", Language: "go"})
	database.UpsertEdge(&db.Edge{SourceID: fooID, TargetID: barID, Kind: db.EdgeCalls, File: fooFile, Line: 1})
	database.UpsertEdge(&db.Edge{SourceID: mainID, TargetID: fooID, Kind: db.EdgeCalls, File: mainFile, Line: 1})
	database.UpsertFile(fooFile, 10, 1)
	database.UpsertFile(barFile, 10, 1)
	database.UpsertFile(mainFile, 10, 1)

	return database, dir, func() { database.Close() }
}

func TestToolCallersGraph(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	text, ok, err := ToolCallersGraph(context.Background(), database, dir, GraphQueryArgs{Name: "foo"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected graph hit")
	}
	if !strings.Contains(text, "main") {
		t.Fatalf("expected main as caller, got:\n%s", text)
	}
}

func TestToolCalleesGraph(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	text, ok, err := ToolCalleesGraph(context.Background(), database, dir, GraphQueryArgs{Name: "foo"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !strings.Contains(text, "bar") {
		t.Fatalf("expected bar callee, ok=%v text=%s", ok, text)
	}
}

func TestToolImpactGraph(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	text, ok, err := ToolImpactGraph(context.Background(), database, dir, GraphQueryArgs{Name: "bar", Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected impact")
	}
	if !strings.Contains(text, "foo.go") && !strings.Contains(text, "main.go") {
		t.Fatalf("expected affected files, got:\n%s", text)
	}
}

func TestToolExploreQuery(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Query: "foo", Max: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "foo") || !strings.Contains(text, "Source") && !strings.Contains(text, "func foo") {
		// body should appear
		if !strings.Contains(text, "func foo") {
			t.Fatalf("explore missing source:\n%s", text)
		}
	}
}

func TestToolExploreOverview(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()
	// create a marker file so overview has something
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)

	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Max: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "go.mod") && !strings.Contains(text, "Top-level") {
		t.Fatalf("unexpected overview:\n%s", text)
	}
}

func TestResolveDefs(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	// Basic lookup
	defs, err := resolveDefs(context.Background(), database, "foo", "", "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 {
		t.Fatal("expected foo defs")
	}
	if defs[0].Name != "foo" {
		t.Fatalf("expected foo, got %s", defs[0].Name)
	}

	// Non-existent symbol
	defs, err = resolveDefs(context.Background(), database, "nonexistent", "", "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 0 {
		t.Fatalf("expected 0 defs, got %d", len(defs))
	}

	// Path filter: only files under foo.go's dir
	defs, err = resolveDefs(context.Background(), database, "foo", dir, "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range defs {
		if d.Name == "foo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("path filter should include foo, got %v", defs)
	}

	// Path filter: exclude by path
	defs, err = resolveDefs(context.Background(), database, "foo", "/nonexistent", "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 0 {
		t.Fatalf("path filter should exclude, got %d", len(defs))
	}

	// Glob filter
	defs, err = resolveDefs(context.Background(), database, "foo", "", "", "*.go", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 {
		t.Fatal("glob *.go should match foo.go")
	}

	defs, err = resolveDefs(context.Background(), database, "foo", "", "", "*.ts", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 0 {
		t.Fatalf("glob *.ts should not match, got %d", len(defs))
	}

	// File hint narrows overloaded names
	defs, err = resolveDefs(context.Background(), database, "foo", "", "foo.go", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "foo" {
		t.Fatalf("file hint should narrow to foo, got %v", defs)
	}
}

func TestTrimExploreBody(t *testing.T) {
	// Short body unchanged
	short := "func foo() {}"
	if got := trimExploreBody(short, 100); got != short {
		t.Fatalf("short body should be unchanged, got %q", got)
	}

	// Empty max returns empty
	if got := trimExploreBody(short, 0); got != "" {
		t.Fatalf("zero max should return empty, got %q", got)
	}

	// Long body gets trimmed
	long := strings.Repeat("x", 200)
	got := trimExploreBody(long, 50)
	if len(got) >= len(long) {
		t.Fatalf("expected trim, got %d chars: %q", len(got), got)
	}
	if !strings.Contains(got, "trimmed") {
		t.Fatalf("trim note missing: %q", got)
	}
}

func TestContainerMatches(t *testing.T) {
	database, _, cleanup := seedGraph(t)
	defer cleanup()

	// Get a node to test with
	defs, err := database.GetNodeByName("foo")
	if err != nil || len(defs) == 0 {
		t.Fatal("expected foo node")
	}
	node := defs[0]

	// Should match when name is in pool
	segPool := map[string]bool{"foo": true}
	if !containerMatches(node, segPool) {
		t.Fatal("foo node should match pool with 'foo'")
	}

	// Should not match when name is not in pool
	segPool = map[string]bool{"nonexistent": true}
	if containerMatches(node, segPool) {
		t.Fatal("foo node should not match pool with 'nonexistent'")
	}
}

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
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

	text, ok, err := ToolCallersGraph(database, dir, GraphQueryArgs{Name: "foo"})
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

	text, ok, err := ToolCalleesGraph(database, dir, GraphQueryArgs{Name: "foo"})
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

	text, ok, err := ToolImpactGraph(database, dir, GraphQueryArgs{Name: "bar", Depth: 3})
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

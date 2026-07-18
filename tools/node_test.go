package tools

import (
	"context"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
)

func setupTestDB(t *testing.T) (*db.DB, func()) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return database, func() { database.Close() }
}

func TestToolNodeByName(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert test data
	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "hello", File: "/a.go", Line: 10, EndLine: 20,
		Body: "func hello() {}", Language: "go",
	})
	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "hello", File: "/b.go", Line: 5, EndLine: 15,
		Body: "func hello() {}", Language: "go",
	})

	result, err := ToolNode(context.Background(), database, NodeArgs{Name: "hello"})
	if err != nil {
		t.Fatalf("tool node: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}

	text := result.Content[0].Text
	if text == "no symbols found" {
		t.Fatal("expected symbols found")
	}
}

func TestToolNodeByFileLine(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "foo", File: "/a.go", Line: 10, EndLine: 20,
		Body: "func foo() {}", Language: "go",
	})

	result, err := ToolNode(context.Background(), database, NodeArgs{File: "/a.go", Line: 15})
	if err != nil {
		t.Fatalf("tool node: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}

	text := result.Content[0].Text
	if text == "no symbols found" {
		t.Fatal("expected symbols found")
	}
}

func TestToolNodeNotFound(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	result, err := ToolNode(context.Background(), database, NodeArgs{Name: "nonexistent"})
	if err != nil {
		t.Fatalf("tool node: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}

	text := result.Content[0].Text
	if text != "no symbols found" {
		t.Fatalf("expected 'no symbols found', got %q", text)
	}
}

func TestToolNodeNoArgs(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	_, err := ToolNode(context.Background(), database, NodeArgs{})
	if err == nil {
		t.Fatal("expected error for empty args")
	}
}

func TestToolNodeWithCallersAndCallees(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	id1, _ := database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "caller", File: "/a.go", Line: 1, EndLine: 10,
		Body: "func caller() { callee() }", Language: "go",
	})
	id2, _ := database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "callee", File: "/b.go", Line: 1, EndLine: 5,
		Body: "func callee() {}", Language: "go",
	})
	database.UpsertEdge(&db.Edge{
		SourceID: id1, TargetID: id2, Kind: db.EdgeCalls, File: "/a.go", Line: 3,
	})

	result, err := ToolNode(context.Background(), database, NodeArgs{Name: "callee"})
	if err != nil {
		t.Fatalf("tool node: %v", err)
	}

	text := result.Content[0].Text
	if text == "no symbols found" {
		t.Fatal("expected symbols found")
	}
	// Should mention caller
	if len(text) > 0 && !contains(text, "caller") {
		t.Error("expected caller in output")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

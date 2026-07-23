package tools

import (
	"context"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestToolStatus(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert test data
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "foo", File: "/a.go", Line: 1})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "bar", File: "/b.go", Line: 1})
	database.UpsertNode(&db.Node{Kind: db.KindClass, Name: "MyClass", File: "/c.go", Line: 1})
	database.UpsertFile("/a.go", 100, 1000.0)
	database.UpsertFile("/b.go", 200, 2000.0)

	result, err := ToolStatus(context.Background(), database, []string{"/workdir"}, "/workdir", StatusArgs{}, nil)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}

	text := result.Content[0].Text
	if text == "" {
		t.Fatal("expected non-empty output")
	}

	// Check for key content
	if !contains(text, "Nodes:") {
		t.Error("expected Nodes in output")
	}
	if !contains(text, "Files:") {
		t.Error("expected Files in output")
	}
}

func TestToolStatusWithPendingFiles(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	pending := []string{"/a.go", "/b.go"}
	result, err := ToolStatus(context.Background(), database, []string{"/workdir"}, "/workdir", StatusArgs{}, pending)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}

	text := result.Content[0].Text
	if !contains(text, "Pending") {
		t.Error("expected Pending in output")
	}
	if !contains(text, "2 files") {
		t.Error("expected 2 files in pending")
	}
}

func TestToolStatusWithFileCheck(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.UpsertFile("/workdir/main.go", 100, 1000.0)

	result, err := ToolStatus(context.Background(), database, []string{"/workdir"}, "/workdir", StatusArgs{Path: "main.go"}, nil)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}

	text := result.Content[0].Text
	if !contains(text, "indexed") {
		t.Error("expected indexed in output")
	}
}

func TestToolStatusWithNonexistentFile(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	result, err := ToolStatus(context.Background(), database, []string{"/workdir"}, "/workdir", StatusArgs{Path: "nonexistent.go"}, nil)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}

	text := result.Content[0].Text
	if !contains(text, "not indexed") {
		t.Error("expected not indexed in output")
	}
}

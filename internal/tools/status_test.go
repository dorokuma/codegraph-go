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

// TestToolStatusPerRootWorkdirSelfReference: in per-root mode the workdir IS
// the project directory and stored files are workdir-relative keys
// (cmd/…, internal/…) that never contain the workdir's own name. A query for
// the project itself — its basename, the absolute workdir path, or "." —
// must report indexed (regression: it used to match zero rows and misreport
// "not indexed").
func TestToolStatusPerRootWorkdirSelfReference(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Per-root storage keys: relative to the workdir, no project-name prefix.
	database.UpsertFile("cmd/main.go", 100, 1000.0)
	database.UpsertFile("internal/tools/status.go", 200, 2000.0)

	workdir := "/repo/codegraph-go"

	// Basename query (the real per-root failure case).
	result, err := ToolStatus(context.Background(), database, []string{workdir}, workdir, StatusArgs{Path: "codegraph-go"}, nil)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}
	if !contains(result.Content[0].Text, "codegraph-go: indexed") {
		t.Errorf("expected workdir basename query to report indexed, got:\n%s", result.Content[0].Text)
	}

	// The workdir itself, addressed absolutely or as ".", also indexed.
	for _, p := range []string{workdir, "."} {
		result, err = ToolStatus(context.Background(), database, []string{workdir}, workdir, StatusArgs{Path: p}, nil)
		if err != nil {
			t.Fatalf("tool status(%q): %v", p, err)
		}
		if !contains(result.Content[0].Text, p+": indexed") {
			t.Errorf("expected %q to report indexed, got:\n%s", p, result.Content[0].Text)
		}
	}

	// A path that is neither the workdir nor a stored key stays not indexed.
	result, err = ToolStatus(context.Background(), database, []string{workdir}, workdir, StatusArgs{Path: "nowhere"}, nil)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}
	if !contains(result.Content[0].Text, "nowhere: not indexed") {
		t.Errorf("expected unrelated query to report not indexed, got:\n%s", result.Content[0].Text)
	}
}

// TestToolStatusMainLibProjectPrefix: home/main-lib mode (workdir = parent)
// stores files under a project-name prefix. Querying the project name must
// keep reporting indexed (regression guard for the existing prefix match).
func TestToolStatusMainLibProjectPrefix(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.UpsertFile("codegraph-go/cmd/main.go", 100, 1000.0)
	database.UpsertFile("codegraph-go/internal/tools/status.go", 200, 2000.0)

	workdir := "/root"
	result, err := ToolStatus(context.Background(), database, []string{workdir}, workdir, StatusArgs{Path: "codegraph-go"}, nil)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}
	if !contains(result.Content[0].Text, "codegraph-go: indexed") {
		t.Errorf("expected project prefix query to report indexed, got:\n%s", result.Content[0].Text)
	}

	// Unrelated query still reports not indexed.
	result, err = ToolStatus(context.Background(), database, []string{workdir}, workdir, StatusArgs{Path: "somewhere-else"}, nil)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}
	if !contains(result.Content[0].Text, "somewhere-else: not indexed") {
		t.Errorf("expected unrelated query to report not indexed, got:\n%s", result.Content[0].Text)
	}
}

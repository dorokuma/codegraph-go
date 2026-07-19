package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "hello", File: "/a.go", Line: 10, EndLine: 20,
		Body: "func hello() {}", Language: "go",
	})
	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "hello", File: "/b.go", Line: 5, EndLine: 15,
		Body: "func hello() { return }", Language: "go",
	})

	true := true
	result, err := ToolNode(context.Background(), database, NodeArgs{Name: "hello", IncludeCode: &true})
	if err != nil {
		t.Fatalf("tool node: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}

	text := result.Content[0].Text
	if strings.Contains(text, "not found") {
		t.Fatal("expected symbols found")
	}
	// Multi-overload: both bodies in one call
	if !strings.Contains(text, "func hello") {
		t.Fatalf("expected body in output, got:\n%s", text)
	}
}

func TestToolNodeByFileLine(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "foo", File: "/proj/a.go", Line: 10, EndLine: 20,
		Body: "func foo() {}", Language: "go",
	})
	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "foo", File: "/proj/b.go", Line: 10, EndLine: 20,
		Body: "func foo() { other() }", Language: "go",
	})

	// name + file + line pins one overload
	result, err := ToolNode(context.Background(), database, NodeArgs{Name: "foo", File: "a.go", Line: 15})
	if err != nil {
		t.Fatalf("tool node: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}

	text := result.Content[0].Text
	if strings.Contains(text, "not found") {
		t.Fatal("expected symbols found")
	}
	if !strings.Contains(text, "a.go") {
		t.Fatalf("expected a.go pin, got:\n%s", text)
	}
	if strings.Contains(text, "2 definitions") {
		t.Fatalf("should pin to one overload:\n%s", text)
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
	if !strings.Contains(text, "not found") {
		t.Fatalf("expected not found, got %q", text)
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
	if strings.Contains(text, "not found") {
		t.Fatal("expected symbols found")
	}
	if !strings.Contains(text, "caller") && !strings.Contains(text, "Callers:") {
		t.Error("expected caller info")
	}
}

func TestToolNodeFileMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.go")
	body := "package main\n\nfunc Hello() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	database.UpsertFileRecord(&db.FileRecord{Path: src, Language: "go", Size: int64(len(body))})
	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "Hello", File: src, Line: 3, EndLine: 5,
		Body: "func Hello() {\n\tprintln(\"hi\")\n}", Language: "go",
	})

	// File alone → Read-like output
	result, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "hello.go"})
	if err != nil {
		t.Fatalf("file mode: %v", err)
	}
	if !result.FileMode {
		t.Fatal("expected FileMode=true")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "1\tpackage main") {
		t.Fatalf("expected numbered source like Read, got:\n%s", text)
	}
	if !strings.Contains(text, "func Hello") {
		t.Fatalf("expected file body, got:\n%s", text)
	}

	// symbolsOnly
	result, err = ToolNodeIn(context.Background(), database, dir, NodeArgs{File: src, SymbolsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	text = result.Content[0].Text
	if !strings.Contains(text, "Hello") {
		t.Fatalf("expected symbol name in output:\n%s", text)
	}
}

func TestToolNodeFileModeDependents(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	os.WriteFile(a, []byte("package p\nfunc A() {}\n"), 0o644)
	os.WriteFile(b, []byte("package p\nfunc B() { A() }\n"), 0o644)

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	database.UpsertFileRecord(&db.FileRecord{Path: a, Language: "go"})
	database.UpsertFileRecord(&db.FileRecord{Path: b, Language: "go"})
	idA, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "A", File: a, Line: 2, EndLine: 2, Language: "go"})
	idB, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "B", File: b, Line: 2, EndLine: 2, Language: "go"})
	database.UpsertEdge(&db.Edge{SourceID: idB, TargetID: idA, Kind: db.EdgeCalls, File: b, Line: 2})

	result, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "a.go", SymbolsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "b.go") {
		t.Fatalf("expected dependent b.go, got:\n%s", text)
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestToolNodeIncludeCodeFalseMulti(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "dup", File: "/x.go", Line: 1, Body: "func dup() {}", Language: "go",
	})
	database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "dup", File: "/y.go", Line: 1, Body: "func dup() {}", Language: "go",
	})
	f := false
	result, err := ToolNode(context.Background(), database, NodeArgs{Name: "dup", IncludeCode: &f})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if strings.Contains(text, "func dup") {
		t.Fatal("includeCode=false should not emit bodies")
	}
	if !strings.Contains(text, "x.go") || !strings.Contains(text, "y.go") {
		t.Fatalf("expected both files listed:\n%s", text)
	}
}

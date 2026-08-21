package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
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

func TestResolveIndexedFile(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	workdir := "/workspace/myproj"
	files := []string{
		"/workspace/myproj/pkg/util/helper.go",
		"/workspace/myproj/cmd/app/main.go",
		"/workspace/myproj/internal/db/main.go",
		"/outside/workspace/secret.go",
	}
	for _, f := range files {
		if err := database.UpsertFile(f, 100, 1000.0); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()

	// 1. Exact match within workdir
	res, cands, err := resolveIndexedFile(ctx, database, workdir, "pkg/util/helper.go")
	if err != nil || res != "/workspace/myproj/pkg/util/helper.go" || len(cands) != 0 {
		t.Fatalf("exact match: got res=%q cands=%v err=%v", res, cands, err)
	}

	// 2. Basename search with multiple matches -> returns ambiguous candidates
	res, cands, err = resolveIndexedFile(ctx, database, workdir, "main.go")
	if err != nil || res != "" || len(cands) != 2 {
		t.Fatalf("ambiguous search: got res=%q cands=%v err=%v", res, cands, err)
	}

	// 3. File outside workdir is excluded by path scoping
	res, cands, err = resolveIndexedFile(ctx, database, workdir, "secret.go")
	if err != nil || res != "" || len(cands) != 0 {
		t.Fatalf("outside workdir: got res=%q cands=%v err=%v", res, cands, err)
	}

	// 4. Empty hint
	res, cands, err = resolveIndexedFile(ctx, database, workdir, "")
	if err != nil || res != "" || len(cands) != 0 {
		t.Fatalf("empty hint: got res=%q cands=%v err=%v", res, cands, err)
	}
}

func TestToolNodeSymbolsOnlyTruncated(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "funcs.go")
	src := "package p\nfunc F1() {}\nfunc F2() {}\nfunc F3() {}\nfunc F4() {}\n"
	if err := os.WriteFile(filePath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if err := database.UpsertFileRecord(&db.FileRecord{Path: filePath, Language: "go"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("F%d", i)
		if _, err := database.UpsertNode(&db.Node{
			Kind:     db.KindFunction,
			Name:     name,
			File:     filePath,
			Line:     i + 1,
			EndLine:  i + 1,
			Language: "go",
		}); err != nil {
			t.Fatal(err)
		}
	}

	oldCap := db.SetGetNodesByFileCapForTest(2)
	defer db.SetGetNodesByFileCapForTest(oldCap)

	// 1. symbolsOnly=true with cap=2 must report real truncation notice
	res, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "funcs.go", SymbolsOnly: true})
	if err != nil {
		t.Fatalf("ToolNodeIn symbolsOnly: %v", err)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "... (truncated at 2 symbols, more exist)") {
		t.Fatalf("expected truncation notice in symbolsOnly view, got:\n%s", text)
	}
	if !strings.Contains(text, "F1") || !strings.Contains(text, "F2") {
		t.Fatalf("expected first 2 symbols in output, got:\n%s", text)
	}
	if strings.Contains(text, "F3") || strings.Contains(text, "F4") {
		t.Fatalf("expected symbols beyond cap to be omitted, got:\n%s", text)
	}

	// 2. symbolsOnly=false (default source view) remains intact and displays source lines
	resDefault, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "funcs.go", SymbolsOnly: false})
	if err != nil {
		t.Fatalf("ToolNodeIn default view: %v", err)
	}
	defaultText := resDefault.Content[0].Text
	if !strings.Contains(defaultText, "1\tpackage p") || !strings.Contains(defaultText, "func F1") {
		t.Fatalf("expected default source lines in output, got:\n%s", defaultText)
	}
	if strings.Contains(defaultText, "truncated at") {
		t.Fatalf("default source view should not include symbolsOnly truncation notice, got:\n%s", defaultText)
	}
}


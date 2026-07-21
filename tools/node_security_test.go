package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
)

func TestNodeSecurity_FileModeRelativePath(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	os.MkdirAll(srcDir, 0755)
	src := filepath.Join(srcDir, "a.go")
	body := "package main\nfunc A() {}\n"
	os.WriteFile(src, []byte(body), 0644)

	database, _ := db.Open(dir)
	defer database.Close()
	database.UpsertFileRecord(&db.FileRecord{Path: src, Language: "go"})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "A", File: src, Line: 2, Language: "go"})

	r, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "src/a.go"})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].Text
	if strings.Contains(text, "No indexed file") || (strings.Contains(text, "matches") && !strings.Contains(text, "package main")) {
		t.Fatalf("FAIL relative path src/a.go: %s", text)
	}
	if !strings.Contains(text, "package main") {
		t.Fatalf("expected source, got: %s", text)
	}
}

func TestNodeSecurity_BasenameAmbiguous(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "pkg1", "util.go")
	b := filepath.Join(dir, "pkg2", "util.go")
	os.MkdirAll(filepath.Dir(a), 0755)
	os.MkdirAll(filepath.Dir(b), 0755)
	os.WriteFile(a, []byte("package p1\n"), 0644)
	os.WriteFile(b, []byte("package p2\n"), 0644)
	database, _ := db.Open(dir)
	defer database.Close()
	database.UpsertFileRecord(&db.FileRecord{Path: a})
	database.UpsertFileRecord(&db.FileRecord{Path: b})

	r, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "util.go"})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].Text
	if !strings.Contains(text, "matches") {
		t.Fatalf("expected ambiguity message, got: %s", text)
	}
}

func TestNodeSecurity_SubstringFalsePositive(t *testing.T) {
	// "main.go" must NOT match "remain.go" via strings.Contains
	dir := t.TempDir()
	a := filepath.Join(dir, "remain.go")
	os.WriteFile(a, []byte("package r\n"), 0644)
	database, _ := db.Open(dir)
	defer database.Close()
	database.UpsertFileRecord(&db.FileRecord{Path: a})

	r, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].Text
	if strings.Contains(text, "package r") || strings.Contains(text, "1\t") {
		t.Fatalf("BUG: substring false positive matched remain.go for main.go:\n%s", text)
	}
	fmt.Println("substring OK:", text)
}

func TestNodeSecurity_HasSuffixFalsePositive_SymbolPin(t *testing.T) {
	// HasSuffix("/x/ba.go", "a.go") == true — classic bug
	dir := t.TempDir()
	ba := filepath.Join(dir, "ba.go")
	ot := filepath.Join(dir, "other.go")
	os.WriteFile(ba, []byte("package ba\nfunc X(){}\n"), 0644)
	os.WriteFile(ot, []byte("package o\nfunc X(){}\n"), 0644)
	database, _ := db.Open(dir)
	defer database.Close()
	database.UpsertFileRecord(&db.FileRecord{Path: ba})
	database.UpsertFileRecord(&db.FileRecord{Path: ot})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "X", File: ba, Line: 2, Body: "func X(){} //BA"})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "X", File: ot, Line: 2, Body: "func X(){} //OT"})

	r, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{Name: "X", File: "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].Text
	// Should NOT pin solely to ba.go because a.go is not a real path suffix with separator
	if strings.Contains(text, "//BA") && !strings.Contains(text, "//OT") && !strings.Contains(text, "2 definitions") {
		t.Fatalf("BUG: HasSuffix false positive a.go matched ba.go only:\n%s", text)
	}
	fmt.Println("suffix pin:", text[:minInt(300, len(text))])
}

func TestNodeSecurity_FileModePathTraversal(t *testing.T) {
	dir := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.go")
	os.WriteFile(secret, []byte("package secret\n// SECRET_TOKEN=abc\n"), 0644)

	database, _ := db.Open(dir)
	defer database.Close()
	database.UpsertFileRecord(&db.FileRecord{Path: secret, Language: "go"})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "S", File: secret, Line: 1})

	r, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: secret})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].Text
	if strings.Contains(text, "SECRET_TOKEN") {
		t.Fatalf("BUG: path escape — read file outside workdir:\n%s", text)
	}
	fmt.Println("escape OK:", text[:minInt(200, len(text))])
}

func TestNodeSecurity_GraphFileHintSuffix(t *testing.T) {
	dir := t.TempDir()
	database, _ := db.Open(dir)
	defer database.Close()
	ba := filepath.Join(dir, "ba.go")
	ot := filepath.Join(dir, "other.go")
	id1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "Foo", File: ba, Line: 1})
	id2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "Foo", File: ot, Line: 1})
	id3, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "Bar", File: ba, Line: 2})
	database.UpsertEdge(&db.Edge{SourceID: id3, TargetID: id1, Kind: db.EdgeCalls, File: ba, Line: 2})
	id4, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "Baz", File: ot, Line: 2})
	database.UpsertEdge(&db.Edge{SourceID: id4, TargetID: id2, Kind: db.EdgeCalls, File: ot, Line: 2})

	text, ok, err := ToolCallersGraph(context.Background(), database, dir, GraphQueryArgs{Name: "Foo", File: "a.go", MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("graph file=a.go ok=%v\n%s\n", ok, text)
	if ok && strings.Contains(text, "Bar") && !strings.Contains(text, "Baz") {
		t.Fatalf("BUG: file=a.go wrongly matched ba.go only:\n%s", text)
	}
}

func TestNodeSecurity_FileModeDotDot(t *testing.T) {
	// Agent passes ../outside
	parent := t.TempDir()
	dir := filepath.Join(parent, "proj")
	os.MkdirAll(dir, 0755)
	outside := filepath.Join(parent, "out.go")
	os.WriteFile(outside, []byte("package out\n// OUTSIDE\n"), 0644)
	inside := filepath.Join(dir, "in.go")
	os.WriteFile(inside, []byte("package in\n"), 0644)

	database, _ := db.Open(dir)
	defer database.Close()
	// index outside via relative escape path as stored? use real path
	database.UpsertFileRecord(&db.FileRecord{Path: outside})
	// also try requesting via ../out.go
	r, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "../out.go"})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].Text
	fmt.Println("dotdot:", text[:minInt(250, len(text))])
	if strings.Contains(text, "OUTSIDE") {
		t.Fatalf("BUG: ../ escape read outside:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

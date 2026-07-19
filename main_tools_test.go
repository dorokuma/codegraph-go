package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func boolPtr(b bool) *bool { return &b }

// textContent extracts text from the first content item.
func textContent(r *mcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func setupToolServer(t *testing.T) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(); os.RemoveAll(dir) })

	// Insert test nodes and edges
	idA, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "Alpha", File: "alpha.go", Line: 1, EndLine: 10, Language: "go", Body: "func Alpha() {}", Signature: "func Alpha()"})
	idB, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "Beta", File: "beta.go", Line: 5, EndLine: 15, Language: "go", Body: "func Beta() {}", Signature: "func Beta()"})
	idC, _ := database.UpsertNode(&db.Node{Kind: db.KindStruct, Name: "Gamma", File: "alpha.go", Line: 20, EndLine: 30, Language: "go", Body: "type Gamma struct {}", Signature: "type Gamma struct"})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "Delta", File: "sub/delta.go", Line: 1, EndLine: 5, Language: "go", Body: "func Delta() {}", Signature: "func Delta()"})

	database.UpsertEdge(&db.Edge{SourceID: idA, TargetID: idB, Kind: db.EdgeCalls, File: "alpha.go", Line: 3})
	database.UpsertEdge(&db.Edge{SourceID: idB, TargetID: idC, Kind: db.EdgeReferences, File: "beta.go", Line: 8})
	database.UpsertEdge(&db.Edge{SourceID: idA, TargetID: idC, Kind: db.EdgeCalls, File: "alpha.go", Line: 5})

	database.UpsertFileRecord(&db.FileRecord{Path: "alpha.go", Size: 500, Language: "go", NodeCount: 2})
	database.UpsertFileRecord(&db.FileRecord{Path: "beta.go", Size: 300, Language: "go", NodeCount: 1})
	database.UpsertFileRecord(&db.FileRecord{Path: "sub/delta.go", Size: 100, Language: "go", NodeCount: 1})

	s := &server{
		workdir:  dir,
		database: database,
	}
	return s, dir
}

func TestToolExploreOverview(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolExplore(context.Background(), nil, exploreArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go") && !strings.Contains(text, "Explore") {
		t.Fatalf("expected alpha.go or Explore in overview, got:\n%s", text)
	}
}

func TestToolExploreQuery(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolExplore(context.Background(), nil, exploreArgs{Query: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "Alpha") {
		t.Fatalf("expected Alpha in result, got:\n%s", text)
	}
}

func TestToolExploreQueryNotFound(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolExplore(context.Background(), nil, exploreArgs{Query: "Nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "not found") && !strings.Contains(text, "no") {
		// It's OK if it returns empty or a "not found" message
		t.Logf("explore result for nonexistent: %s", text)
	}
}

func TestToolNodeByName(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolNode(context.Background(), nil, nodeArgs{Name: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go") {
		t.Fatalf("expected alpha.go in result, got:\n%s", text)
	}
}

func TestToolNodeByFileLine(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolNode(context.Background(), nil, nodeArgs{File: "alpha.go", Line: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go:1") && !strings.Contains(text, "alpha.go") {
		t.Fatalf("expected alpha.go in result, got:\n%s", text)
	}
}

func TestToolNodeNotFound(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolNode(context.Background(), nil, nodeArgs{Name: "Nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "not found") {
		t.Fatalf("expected 'not found', got:\n%s", text)
	}
}

func TestToolNodeNoArgs(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolNode(context.Background(), nil, nodeArgs{})
	if err == nil {
		t.Fatal("expected error for empty args")
	}
}

func TestToolNodeIncludeCodeFalse(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolNode(context.Background(), nil, nodeArgs{Name: "Alpha", IncludeCode: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	// With includeCode=false, body should not be included
	if strings.Contains(text, "func Alpha() {}") {
		t.Fatalf("expected body to be excluded with includeCode=false, got:\n%s", text)
	}
}

func TestToolCallers(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCallers(context.Background(), nil, nameArgs{Name: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go") {
		t.Fatalf("expected alpha.go as caller of Beta, got:\n%s", text)
	}
}

func TestToolCallersNotFound(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCallers(context.Background(), nil, nameArgs{Name: "Nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "not found") && !strings.Contains(text, "no") {
		t.Logf("callers result for nonexistent: %s", text)
	}
}

func TestToolCallees(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCallees(context.Background(), nil, nameArgs{Name: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	if !strings.Contains(text, "beta.go") && !strings.Contains(text, "alpha.go") {
		t.Fatalf("expected beta.go or alpha.go as callee of Alpha, got:\n%s", text)
	}
}

func TestToolImpact(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolImpact(context.Background(), nil, nameArgs{Name: "Gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	// Gamma is referenced from alpha.go and beta.go
	if !strings.Contains(text, "alpha.go") && !strings.Contains(text, "Impact") {
		t.Fatalf("expected alpha.go or Impact in result, got:\n%s", text)
	}
}

func TestToolStatus(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolStatus(context.Background(), nil, statusArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	if !strings.Contains(text, "Nodes") {
		t.Fatalf("expected 'Nodes' in status, got:\n%s", text)
	}
	if !strings.Contains(text, "schema=") {
		t.Fatalf("expected 'schema=' in status, got:\n%s", text)
	}
}

func TestToolFiles(t *testing.T) {
	s, dir := setupToolServer(t)
	// Create actual files on disk for rg to find
	os.MkdirAll(dir, 0o700)
	os.WriteFile(dir+"/alpha.go", []byte("package main"), 0o600)
	result, _, err := s.toolFiles(context.Background(), nil, filesArgs{Pattern: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go") && !strings.Contains(text, "no files") {
		t.Fatalf("expected alpha.go or 'no files' in files result, got:\n%s", text)
	}
}

func TestToolSearch(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go") && !strings.Contains(text, "no files") {
		t.Fatalf("expected alpha.go or 'no files' in search result, got:\n%s", text)
	}
}

func TestToolSearchEmptyPattern(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: ""})
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
}

func TestToolExplorePath(t *testing.T) {
	s, dir := setupToolServer(t)
	// Create sub directory and file on disk
	os.MkdirAll(dir+"/sub", 0o700)
	os.WriteFile(dir+"/sub/delta.go", []byte("package main\nfunc Delta() {}"), 0o600)
	result, _, err := s.toolExplore(context.Background(), nil, exploreArgs{Path: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "delta.go") && !strings.Contains(text, "sub") {
		t.Fatalf("expected delta.go or sub in path result, got:\n%s", text)
	}
}

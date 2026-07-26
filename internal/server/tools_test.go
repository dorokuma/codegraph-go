package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
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

func setupToolServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(); os.RemoveAll(dir) })

	// On-disk files so node file-mode Read works with workdir-relative index keys.
	_ = os.WriteFile(filepath.Join(dir, "alpha.go"), []byte("package p\nfunc Alpha() {}\ntype Gamma struct {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "beta.go"), []byte("package p\nfunc Beta() {}\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sub", "delta.go"), []byte("package sub\nfunc Delta() {}\n"), 0o644)

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

	s := &Server{
		Workdir:  dir,
		Database: database,
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
	// Symbol mode with file/line pin (file alone is whole-file Read mode).
	result, _, err := s.toolNode(context.Background(), nil, nodeArgs{Name: "Alpha", File: "alpha.go", Line: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go") {
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

func TestToolAffectedRejectsStdinOverMCP(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolAffected(context.Background(), nil, affectedArgs{
		Stdin: true,
		Files: []string{"alpha.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("expected MCP stdin rejection, got %v", err)
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

func TestToolStoreFactHappyPath(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile:   "alpha.go",
		TargetSymbol: "Alpha",
		TargetLine:   10,
		Content:      "Alpha is the main entry point",
		Author:       "test-agent",
	})
	if err != nil {
		t.Fatalf("store_fact: %v", err)
	}
	text := textContent(result)
	if !strings.Contains(text, "Alpha is the main entry point") {
		t.Fatalf("expected content in response, got:\n%s", text)
	}
	if strings.Contains(text, `"duplicate":true`) {
		t.Fatalf("unexpected duplicate in response:\n%s", text)
	}
}

func TestToolStoreFactDuplicate(t *testing.T) {
	s, _ := setupToolServer(t)
	// Store once
	s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile: "alpha.go",
		Content:    "Same content",
		Author:     "agent1",
	})
	// Store same content again
	result, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile: "beta.go",
		Content:    "Same content",
		Author:     "agent2",
	})
	if err != nil {
		t.Fatalf("second store_fact: %v", err)
	}
	text := textContent(result)
	if !strings.Contains(text, `"duplicate":true`) {
		t.Fatalf("expected duplicate=true in response:\n%s", text)
	}
}

func TestToolStoreFactRequiredFields(t *testing.T) {
	s, _ := setupToolServer(t)
	// Missing targetFile
	_, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		Content: "some fact",
	})
	if err == nil {
		t.Fatal("expected error for missing targetFile")
	}

	// Missing content
	_, _, err = s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile: "a.go",
	})
	if err == nil {
		t.Fatal("expected error for missing content")
	}
}

func TestToolSearchFacts(t *testing.T) {
	s, _ := setupToolServer(t)
	// Insert a couple facts via DB directly
	s.Database.InsertFact(&db.Fact{
		TargetFile: "a.go", TargetSymbol: "Foo",
		Content: "Foo handles requests", ContentHash: "h1", Status: "active",
	})
	s.Database.InsertFact(&db.Fact{
		TargetFile: "b.go", TargetSymbol: "Bar",
		Content: "Bar does validation", ContentHash: "h2", Status: "active",
	})

	// Search by content substring
	result, _, err := s.toolSearchFacts(context.Background(), nil, searchFactsArgs{
		Query: "handles",
	})
	if err != nil {
		t.Fatalf("search_facts: %v", err)
	}
	text := textContent(result)
	if !strings.Contains(text, "Foo handles requests") {
		t.Fatalf("expected 'Foo handles requests' in result, got:\n%s", text)
	}
}

func TestToolSearchFactsNotFound(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolSearchFacts(context.Background(), nil, searchFactsArgs{
		Query: "nonexistent",
	})
	if err != nil {
		t.Fatalf("search_facts: %v", err)
	}
	text := textContent(result)
	if !strings.Contains(text, "no facts found") {
		t.Fatalf("expected 'no facts found', got:\n%s", text)
	}
}

func TestToolStoreFactWithSupersedes(t *testing.T) {
	s, _ := setupToolServer(t)

	// Store first fact
	res1, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile: "alpha.go",
		Content:    "Old version",
	})
	if err != nil {
		t.Fatalf("first store: %v", err)
	}

	// Parse the id from the response JSON
	// The response is JSON: {"duplicate":false,"fact":{...},"same_target":[...]}
	type storeResp struct {
		Duplicate bool     `json:"duplicate"`
		Fact      *db.Fact `json:"fact"`
	}
	var r1 storeResp
	if err := json.Unmarshal([]byte(textContent(res1)), &r1); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	oldID := r1.Fact.ID

	// Store superseding fact
	res2, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile: "alpha.go",
		Content:    "New version",
		Supersedes: oldID,
	})
	if err != nil {
		t.Fatalf("second store: %v", err)
	}

	// Verify old fact is now superseded
	oldFact, _ := s.Database.GetFactByHash(r1.Fact.ContentHash)
	if oldFact.Status != "superseded" {
		t.Fatalf("expected old fact to be superseded, got %q", oldFact.Status)
	}

	_ = res2
}

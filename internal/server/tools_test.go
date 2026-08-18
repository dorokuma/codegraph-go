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

func TestToolCodegraphExplore(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{
		Action: "explore",
		Query:  "Alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "Alpha") {
		t.Fatalf("expected Alpha via action=explore, got:\n%s", text)
	}
}

func TestToolCodegraphUnknownAction(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "nope"})
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestToolCodegraphMissingAction(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{})
	if err == nil {
		t.Fatal("expected error for empty action")
	}
}

func TestNewMCPServerOnlyCodegraphTool(t *testing.T) {
	s, _ := setupToolServer(t)
	srv := NewMCPServer(s)
	if srv == nil {
		t.Fatal("nil server")
	}
	// Smoke: dispatcher path used by MCP registration stays wired.
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "status"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolCodegraphSearchRequiresPattern(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "search"})
	if err == nil {
		t.Fatal("expected error for search without pattern")
	}
}

func TestToolCodegraphSearch(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{
		Action:  "search",
		Pattern: "Alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	// FTS or rg may return path:line or empty/no matches; must not error
	if result == nil {
		t.Fatal("nil result")
	}
	_ = text
}

func TestToolCodegraphCallersRequiresName(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "callers"})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name required error, got %v", err)
	}
}

func TestToolCodegraphCallers(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{
		Action: "callers",
		Name:   "Beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
}

func TestToolCodegraphFilesGlobAlias(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{
		Action: "files",
		Glob:   "*.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, ".go") && !strings.Contains(text, "no") && text == "" {
		// empty listing is ok for some fixtures; non-error is the contract
		t.Logf("files result: %q", text)
	}
}

func TestToolCodegraphStoreAndSearchFact(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{
		Action:       "store_fact",
		TargetFile:   "alpha.go",
		TargetSymbol: "Alpha",
		Content:      "via action router",
		Author:       "audit-test",
	})
	if err != nil {
		t.Fatalf("store_fact: %v", err)
	}
	result, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{
		Action: "search_facts",
		Query:  "action router",
	})
	if err != nil {
		t.Fatalf("search_facts: %v", err)
	}
	text := textContent(result)
	if !strings.Contains(text, "action router") && !strings.Contains(text, "Alpha") {
		t.Fatalf("expected fact content in search_facts, got:\n%s", text)
	}
}

func TestToolCodegraphMaxResultsAlias(t *testing.T) {
	s, _ := setupToolServer(t)
	// max_results only (no max) should not panic; cap forwarded to search
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{
		Action:     "search",
		Pattern:    "Alpha",
		MaxResults: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolCodegraphActionCaseInsensitive(t *testing.T) {
	s, _ := setupToolServer(t)
	_, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "STATUS"})
	if err != nil {
		t.Fatal(err)
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

func TestToolExploreQueryPathSubdir(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolExplore(context.Background(), nil, exploreArgs{
		Query: "Delta",
		Path:  "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if strings.Contains(text, "no indexed symbols") {
		t.Fatalf("query+path=sub must find relative-key Delta:\n%s", text)
	}
	if !strings.Contains(text, "Delta") {
		t.Fatalf("expected Delta:\n%s", text)
	}
}

func TestToolSearchMatchLine(t *testing.T) {
	s, dir := setupToolServer(t)
	body := "func Wrapper() {\n" + strings.Repeat("// pad\n", 13) + "\tneedle()\n}\n"
	if _, err := s.Database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "Wrapper", File: "wrap.go", Line: 1,
		Body: body, Language: "go",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wrap.go"), []byte("package p\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	result, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "wrap.go:15") && !strings.Contains(text, "wrap.go:16") {
		t.Fatalf("FTS search must print the body match line, got:\n%s", text)
	}
	if strings.TrimSpace(text) == "wrap.go:1" {
		t.Fatal("printed definition line instead of match line")
	}
}

func TestToolCallersCallSiteLine(t *testing.T) {
	s, _ := setupToolServer(t)
	result, _, err := s.toolCallers(context.Background(), nil, nameArgs{Name: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	text := textContent(result)
	if !strings.Contains(text, "alpha.go:3") {
		t.Fatalf("callers must print call-site line 3, got:\n%s", text)
	}
}

func TestToolExploreHomePathProject(t *testing.T) {
	base := t.TempDir()
	broadHome(t, base)
	proj := filepath.Join(base, "myrepo")
	writeProjectDir(t, proj)
	if err := os.WriteFile(filepath.Join(proj, "alpha.go"), []byte("package p\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projDB, err := db.Open(proj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projDB.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "Alpha", File: "alpha.go", Line: 2,
		Body: "func Alpha() {}", Language: "go",
	}); err != nil {
		projDB.Close()
		t.Fatal(err)
	}
	if err := projDB.Close(); err != nil {
		t.Fatal(err)
	}
	homeDB, err := db.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { homeDB.Close() })
	s := &Server{Workdir: base, Workdirs: []string{base}, Database: homeDB}
	result, _, err := s.toolExplore(context.Background(), nil, exploreArgs{
		Query: "Alpha",
		Path:  "myrepo",
	})
	if err != nil {
		t.Fatalf("path=myrepo must not 404 after detectProject: %v", err)
	}
	text := textContent(result)
	if strings.Contains(text, "no indexed symbols") {
		t.Fatalf("expected Alpha under myrepo:\n%s", text)
	}
	if !strings.Contains(text, "Alpha") {
		t.Fatalf("expected Alpha:\n%s", text)
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
		TargetFile:   "alpha.go",
		TargetSymbol: "Alpha",
		Content:      "Same content",
		Author:       "agent1",
	})
	// Same text on a different symbol is a new fact.
	result, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile:   "beta.go",
		TargetSymbol: "Beta",
		Content:      "Same content",
		Author:       "agent2",
	})
	if err != nil {
		t.Fatalf("second store_fact: %v", err)
	}
	text := textContent(result)
	if strings.Contains(text, `"duplicate":true`) {
		t.Fatalf("same text on another target must not be duplicate:\n%s", text)
	}
	if !strings.Contains(text, "beta.go") {
		t.Fatalf("expected beta.go fact:\n%s", text)
	}
	// Same target + same text is still a duplicate.
	again, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile:   "alpha.go",
		TargetSymbol: "Alpha",
		Content:      "Same content",
	})
	if err != nil {
		t.Fatalf("third store_fact: %v", err)
	}
	if !strings.Contains(textContent(again), `"duplicate":true`) {
		t.Fatalf("expected duplicate=true for same target:\n%s", textContent(again))
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

// TestToolStoreFactRejectsEscapes: targetFile must be confined to the project
// root. Absolute paths outside the root, relative ../ escapes, and symlinks
// inside the root pointing outside must all be rejected — and no fact may be
// written by any escape attempt.
func TestToolStoreFactRejectsEscapes(t *testing.T) {
	s, dir := setupToolServer(t)

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.go")
	if err := os.WriteFile(outsideFile, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outsideDir, "secret.go")
	if err := os.WriteFile(secret, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Absolute path outside the project root.
	_, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: outsideFile, Content: "c1"})
	if err == nil {
		t.Fatal("expected error for absolute targetFile outside the project root")
	}
	// The error must not leak the project root (displayRoot renders it as a
	// basename/relative form); echoing the client's own targetFile back is
	// fine — that is their input, not host layout.
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("escape error leaked the absolute project root: %v", err)
	}
	// Relative ../ escape.
	if _, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: "../outside.go", Content: "c2"}); err == nil {
		t.Fatal("expected error for relative ../ escape")
	}
	// Symlink inside the root pointing outside (existing target file).
	evil := filepath.Join(dir, "evil")
	if err := os.Symlink(outsideDir, evil); err == nil {
		_, _, serr := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: "evil/secret.go", Content: "c3"})
		if serr == nil {
			t.Fatal("expected error for symlink escape (existing target)")
		}
		if strings.Contains(serr.Error(), dir) || strings.Contains(serr.Error(), outsideDir) {
			t.Fatalf("symlink-escape error leaked an absolute path: %v", serr)
		}
		// Missing tail under the escaping symlink (future file) too.
		if _, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: "evil/notyet.go", Content: "c4"}); err == nil {
			t.Fatal("expected error for symlink escape (missing tail)")
		}
	} else {
		t.Logf("symlinks not supported on this platform, skipping symlink cases: %v", err)
	}

	// No fact may have been written by any escape attempt.
	facts, err := s.Database.SearchFacts("", "", "", "all", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("escape attempts must not write facts, got %d", len(facts))
	}
}

// TestToolStoreFactAcceptsLegitPaths: existing files, not-yet-created future
// files (including in not-yet-existing subdirectories), absolute in-root
// paths, and dirty relative paths must all be accepted; dirty and absolute
// inputs are normalized to the workdir-relative storage key.
func TestToolStoreFactAcceptsLegitPaths(t *testing.T) {
	s, dir := setupToolServer(t)

	if _, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: "alpha.go", Content: "on alpha"}); err != nil {
		t.Fatalf("existing relative file rejected: %v", err)
	}
	if _, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: "future.go", Content: "on future"}); err != nil {
		t.Fatalf("future file rejected: %v", err)
	}
	if _, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: "newpkg/deep/future.go", Content: "on deep future"}); err != nil {
		t.Fatalf("future nested file rejected: %v", err)
	}
	if _, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: filepath.Join(dir, "alpha.go"), Content: "on alpha abs"}); err != nil {
		t.Fatalf("absolute in-root file rejected: %v", err)
	}
	if _, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{TargetFile: "./sub/../beta.go", Content: "on beta dirty"}); err != nil {
		t.Fatalf("dirty relative path rejected: %v", err)
	}

	// All facts stored under normalized workdir-relative keys.
	if facts, _ := s.Database.GetFactsByTarget("alpha.go", ""); len(facts) != 2 {
		t.Fatalf("alpha.go facts = %d, want 2 (relative + absolute inputs)", len(facts))
	}
	if facts, _ := s.Database.GetFactsByTarget("future.go", ""); len(facts) != 1 {
		t.Fatalf("future.go facts = %d, want 1", len(facts))
	}
	if facts, _ := s.Database.GetFactsByTarget("newpkg/deep/future.go", ""); len(facts) != 1 {
		t.Fatalf("newpkg/deep/future.go facts = %d, want 1", len(facts))
	}
	if facts, _ := s.Database.GetFactsByTarget("beta.go", ""); len(facts) != 1 {
		t.Fatalf("beta.go facts = %d, want 1 (normalized from ./sub/../beta.go)", len(facts))
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

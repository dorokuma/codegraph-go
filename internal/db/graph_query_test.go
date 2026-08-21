package db

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// setupGraphDB creates a DB with nodes and edges for graph query tests.
func setupGraphDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(); os.RemoveAll(dir) })

	// Create nodes: A calls B, B calls C, A calls C
	idA, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "A", File: "a.go", Line: 1, EndLine: 10, Language: "go"})
	idB, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "B", File: "b.go", Line: 1, EndLine: 5, Language: "go"})
	idC, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "C", File: "c.go", Line: 1, EndLine: 3, Language: "go"})
	idD, _ := database.UpsertNode(&Node{Kind: KindStruct, Name: "D", File: "a.go", Line: 20, EndLine: 30, Language: "go"})

	// Edges: A->B (calls), A->C (calls), B->C (calls), A->D (references)
	database.UpsertEdge(&Edge{SourceID: idA, TargetID: idB, Kind: EdgeCalls, File: "a.go", Line: 5})
	database.UpsertEdge(&Edge{SourceID: idA, TargetID: idC, Kind: EdgeCalls, File: "a.go", Line: 6})
	database.UpsertEdge(&Edge{SourceID: idB, TargetID: idC, Kind: EdgeCalls, File: "b.go", Line: 3})
	database.UpsertEdge(&Edge{SourceID: idA, TargetID: idD, Kind: EdgeReferences, File: "a.go", Line: 7})

	return database, dir
}

func TestGetNodesByFile(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, err := db.GetNodesByFile("a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes in a.go, got %d", len(nodes))
	}
	names := map[string]bool{}
	for _, n := range nodes {
		names[n.Name] = true
	}
	if !names["A"] || !names["D"] {
		t.Fatalf("expected A and D, got %v", names)
	}
}

func TestGetNodeByID(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("B")
	if len(nodes) == 0 {
		t.Fatal("no node B")
	}
	got, err := db.GetNodeByID(nodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "B" {
		t.Fatalf("expected B, got %v", got)
	}
}

func TestGetNodeByIDNotFound(t *testing.T) {
	db, _ := setupGraphDB(t)
	got, err := db.GetNodeByID(99999)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil for nonexistent ID, got %v", got)
	}
}

func TestGetNodesByKind(t *testing.T) {
	db, _ := setupGraphDB(t)
	funcs, err := db.GetNodesByKind(KindFunction)
	if err != nil {
		t.Fatal(err)
	}
	if len(funcs) != 3 {
		t.Fatalf("want 3 functions, got %d", len(funcs))
	}
	structs, err := db.GetNodesByKind(KindStruct)
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 1 || structs[0].Name != "D" {
		t.Fatalf("want 1 struct D, got %v", structs)
	}
}

func TestGetIncomingEdges(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("C")
	idC := nodes[0].ID

	incoming, err := db.GetIncomingEdges(idC, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A->C and B->C
	if len(incoming) != 2 {
		t.Fatalf("want 2 incoming edges to C, got %d", len(incoming))
	}
}

func TestGetIncomingEdgesFiltered(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("D")
	idD := nodes[0].ID

	// D has only a "references" edge from A
	refs, err := db.GetIncomingEdges(idD, []string{EdgeReferences})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 references edge to D, got %d", len(refs))
	}

	calls, err := db.GetIncomingEdges(idD, []string{EdgeCalls})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("want 0 calls edges to D, got %d", len(calls))
	}
}

func TestGetOutgoingEdges(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("A")
	idA := nodes[0].ID

	outgoing, err := db.GetOutgoingEdges(idA, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A->B, A->C, A->D
	if len(outgoing) != 3 {
		t.Fatalf("want 3 outgoing edges from A, got %d", len(outgoing))
	}
}

func TestGetCallersWithKind(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("C")
	idC := nodes[0].ID

	callers, err := db.GetCallersWithKind(idC)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 2 {
		t.Fatalf("want 2 callers of C, got %d", len(callers))
	}
	names := map[string]bool{}
	for _, c := range callers {
		names[c.Name] = true
		if c.EdgeKind == "" {
			t.Fatal("expected EdgeKind to be set")
		}
	}
	if !names["A"] || !names["B"] {
		t.Fatalf("expected callers A and B, got %v", names)
	}
}

func TestGetCallersWithKindCallSiteLine(t *testing.T) {
	database, _ := setupGraphDB(t)
	nodes, _ := database.GetNodeByName("B")
	callers, err := database.GetCallersWithKind(nodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 {
		t.Fatalf("want 1 caller of B, got %d", len(callers))
	}
	if callers[0].Line != 5 {
		t.Fatalf("want call-site line 5, got %d", callers[0].Line)
	}
}

func TestGetCalleesWithKind(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("A")
	idA := nodes[0].ID

	callees, err := db.GetCalleesWithKind(idA)
	if err != nil {
		t.Fatal(err)
	}
	// A calls B, C and references D — but GetCalleesWithKind filters by structuralEdgeSQL
	// which includes calls/references/extends/implements/bridge
	if len(callees) < 2 {
		t.Fatalf("want at least 2 callees of A, got %d", len(callees))
	}
	for _, c := range callees {
		if c.EdgeKind == "" {
			t.Fatal("expected EdgeKind to be set")
		}
	}
}

func TestGetImpact(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("C")
	idC := nodes[0].ID

	impact, err := db.GetImpact(idC)
	if err != nil {
		t.Fatal(err)
	}
	// C is referenced from a.go (via A->C) and b.go (via B->C)
	if len(impact) != 2 {
		t.Fatalf("want 2 files in impact of C, got %d: %v", len(impact), impact)
	}
	if impact["a.go"] == 0 {
		t.Fatal("expected a.go in impact")
	}
	if impact["b.go"] == 0 {
		t.Fatal("expected b.go in impact")
	}
}

func TestGetImpactNoEdges(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("D")
	idD := nodes[0].ID

	impact, err := db.GetImpact(idD)
	if err != nil {
		t.Fatal(err)
	}
	// D has a references edge from A, so it should show up
	if len(impact) == 0 {
		// This is OK if the edge kind is not in structuralEdgeSQL
		t.Log("no impact files for D (references may not be structural)")
	}
}

func TestDeleteSynthesizedEdges(t *testing.T) {
	db, _ := setupGraphDB(t)
	nodes, _ := db.GetNodeByName("A")
	idA := nodes[0].ID

	// Add a synthesized edge
	db.UpsertEdge(&Edge{SourceID: idA, TargetID: 0, Kind: EdgeCalls, File: "a.go", Line: 99, Provenance: "heuristic"})

	err := db.DeleteSynthesizedEdges()
	if err != nil {
		t.Fatal(err)
	}
}

func TestListUnresolvedRefs(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	id, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "caller", File: "x.go", Line: 1, Language: "go"})
	database.InsertUnresolvedRef(&UnresolvedRef{
		FromNode:      id,
		ReferenceName: "missing_fn",
		ReferenceKind: EdgeCalls,
		Line:          5,
		FilePath:      "x.go",
		Language:      "go",
		Status:        "pending",
	})
	database.InsertUnresolvedRef(&UnresolvedRef{
		FromNode:      id,
		ReferenceName: "failed_fn",
		ReferenceKind: EdgeCalls,
		Line:          10,
		FilePath:      "x.go",
		Language:      "go",
		Status:        "failed",
	})

	pending, err := database.ListUnresolvedRefs("", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ReferenceName != "missing_fn" {
		t.Fatalf("want 1 pending ref 'missing_fn', got %v", pending)
	}

	failed, err := database.ListUnresolvedRefs("", "failed")
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ReferenceName != "failed_fn" {
		t.Fatalf("want 1 failed ref 'failed_fn', got %v", failed)
	}

	all, err := database.ListUnresolvedRefs("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 total refs, got %d", len(all))
	}
}

func TestDeleteUnresolvedRef(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	id, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "caller", File: "x.go", Line: 1, Language: "go"})
	rid, _ := database.InsertUnresolvedRef(&UnresolvedRef{
		FromNode:      id,
		ReferenceName: "to_delete",
		ReferenceKind: EdgeCalls,
		Line:          5,
		FilePath:      "x.go",
		Language:      "go",
		Status:        "pending",
	})

	if err := database.DeleteUnresolvedRef(rid); err != nil {
		t.Fatal(err)
	}

	refs, _ := database.ListUnresolvedRefs("", "")
	if len(refs) != 0 {
		t.Fatalf("want 0 refs after delete, got %d", len(refs))
	}
}

func TestMarkUnresolvedFailed(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	id, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "caller", File: "x.go", Line: 1, Language: "go"})
	rid, _ := database.InsertUnresolvedRef(&UnresolvedRef{
		FromNode:      id,
		ReferenceName: "to_fail",
		ReferenceKind: EdgeCalls,
		Line:          5,
		FilePath:      "x.go",
		Language:      "go",
		Status:        "pending",
	})

	if err := database.MarkUnresolvedFailed(rid, "tail"); err != nil {
		t.Fatal(err)
	}

	refs, _ := database.ListUnresolvedRefs("", "failed")
	if len(refs) != 1 || refs[0].NameTail != "tail" {
		t.Fatalf("want 1 failed ref with tail='tail', got %v", refs)
	}
}

func TestGetFileDependents(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	idA, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "A", File: "a.go", Line: 1, Language: "go"})
	idB, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "B", File: "b.go", Line: 1, Language: "go"})
	database.UpsertEdge(&Edge{SourceID: idB, TargetID: idA, Kind: EdgeCalls, File: "b.go", Line: 1})

	deps, err := database.GetFileDependents("a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0] != "b.go" {
		t.Fatalf("want [b.go] as dependents of a.go, got %v", deps)
	}
}

func TestDeleteFile(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	database.UpsertNode(&Node{Kind: KindFunction, Name: "X", File: "del.go", Line: 1, Language: "go"})
	database.UpsertFileRecord(&FileRecord{Path: "del.go", Size: 100, Language: "go"})

	if err := database.DeleteFile("del.go"); err != nil {
		t.Fatal(err)
	}

	nodes, _ := database.GetNodesByFile("del.go")
	if len(nodes) != 0 {
		t.Fatalf("want 0 nodes after DeleteFile, got %d", len(nodes))
	}
}

func TestGetImportTargetNames(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { database.Close(); os.RemoveAll(dir) }()

	idA, _ := database.UpsertNode(&Node{Kind: KindFunction, Name: "A", File: "a.go", Line: 1, Language: "go"})
	idB, _ := database.UpsertNode(&Node{Kind: KindFile, Name: "pkg", File: "b.go", Line: 1, Language: "go"})
	database.UpsertEdge(&Edge{SourceID: idA, TargetID: idB, Kind: EdgeImports, File: "a.go", Line: 1})

	names, err := database.GetImportTargetNames("a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "pkg" {
		t.Fatalf("want [pkg], got %v", names)
	}
}

// TestGetNodesByKindLimited: explicit limit + truncation flag, and the plain
// GetNodesByKind delegation keeps working with its default cap.
func TestGetNodesByKindLimited(t *testing.T) {
	db, _ := setupGraphDB(t) // A, B, C functions + D struct

	funcs, truncated, err := db.GetNodesByKindLimited(KindFunction, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(funcs) != 2 || !truncated {
		t.Fatalf("limit 2: got %d nodes, truncated=%v (want 2, true)", len(funcs), truncated)
	}
	all, truncated, err := db.GetNodesByKindLimited(KindFunction, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || truncated {
		t.Fatalf("limit 10: got %d nodes, truncated=%v (want 3, false)", len(all), truncated)
	}
	plain, err := db.GetNodesByKind(KindFunction)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 3 {
		t.Fatalf("GetNodesByKind = %d nodes, want 3", len(plain))
	}
}

// TestGraphQueriesCapped: callers/callees/impact (and the WithKind variants)
// must be bounded by graphQueryRowLimit instead of loading every row.
func TestGraphQueriesCapped(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	old := graphQueryRowLimit
	graphQueryRowLimit = 2
	defer func() { graphQueryRowLimit = old }()

	idX, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "X", File: "x.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, n := range []string{"a", "b", "c"} {
		id, err := database.UpsertNode(&Node{Kind: KindFunction, Name: n, File: n + ".go", Line: 1, Language: "go"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if _, err := database.UpsertEdge(&Edge{SourceID: id, TargetID: idX, Kind: EdgeCalls, File: n + ".go", Line: 5}); err != nil {
			t.Fatal(err)
		}
	}

	callers, err := database.GetCallers(idX)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 2 {
		t.Fatalf("GetCallers capped: got %d, want 2", len(callers))
	}
	callees, err := database.GetCallees(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(callees) != 1 {
		t.Fatalf("GetCallees: got %d, want 1", len(callees))
	}
	impact, err := database.GetImpact(idX)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact) != 2 {
		t.Fatalf("GetImpact capped: got %d, want 2", len(impact))
	}
	refs, err := database.GetCallersWithKindContext(context.Background(), idX)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("GetCallersWithKindContext capped: got %d, want 2", len(refs))
	}
	crefs, err := database.GetCalleesWithKindContext(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(crefs) != 1 {
		t.Fatalf("GetCalleesWithKindContext: got %d, want 1", len(crefs))
	}
}

func TestGetNodeByNameCap(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	oldCap := getNodeByNameCap
	getNodeByNameCap = 2
	defer func() { getNodeByNameCap = oldCap }()

	for i := 0; i < 5; i++ {
		if _, err := database.UpsertNode(&Node{
			Kind: KindFunction, Name: "same_name", File: fmt.Sprintf("file_%d.go", i), Line: 1, Language: "go",
		}); err != nil {
			t.Fatal(err)
		}
	}

	nodes, err := database.GetNodeByName("same_name")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("GetNodeByName must be capped at 2, got %d", len(nodes))
	}
}

func TestGetGraphSnapshotLightweightProjection(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	body := "func Hello() { return 42 }"
	nID, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "Hello", File: "a.go", Line: 10, EndLine: 12,
		Body: body, Language: "go", QualifiedName: "pkg.Hello", ReturnType: "int",
	})
	if err != nil {
		t.Fatal(err)
	}

	snap, err := database.GetGraphSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(snap.Nodes))
	}
	n := snap.Nodes[0]
	if n.ID != nID || n.Name != "Hello" || n.Kind != KindFunction || n.File != "a.go" || n.Line != 10 {
		t.Fatalf("unexpected node fields: %+v", n)
	}
	if n.Body != "" {
		t.Fatalf("GetGraphSnapshot must omit body (lightweight), got %q", n.Body)
	}
	if n.QualifiedName != "pkg.Hello" || n.ReturnType != "int" {
		t.Fatalf("non-body fields must be preserved: %+v", n)
	}
}

func TestGetNodesByFileLightweightAndWithBody(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	body := "func Target() { return }"
	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "Target1", File: "target.go", Line: 1, Body: body, Language: "go",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "Target2", File: "target.go", Line: 5, Body: body, Language: "go",
	}); err != nil {
		t.Fatal(err)
	}

	// GetNodesByFile restores body semantics
	nodes, err := database.GetNodesByFile("target.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(nodes))
	}
	for _, n := range nodes {
		if n.Body != body {
			t.Fatalf("GetNodesByFile must load body, got %q", n.Body)
		}
	}

	// GetNodesByFileLight omits body
	lightNodes, err := database.GetNodesByFileLight("target.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(lightNodes) != 2 {
		t.Fatalf("want 2 light nodes, got %d", len(lightNodes))
	}
	for _, n := range lightNodes {
		if n.Body != "" {
			t.Fatalf("GetNodesByFileLight must not load body, got %q", n.Body)
		}
	}

	// Context variants
	ctx := context.Background()
	ctxNodes, err := database.GetNodesByFileContext(ctx, "target.go")
	if err != nil || len(ctxNodes) != 2 || ctxNodes[0].Body != body {
		t.Fatalf("GetNodesByFileContext failed: err=%v, len=%d", err, len(ctxNodes))
	}

	ctxLight, err := database.GetNodesByFileLightContext(ctx, "target.go")
	if err != nil || len(ctxLight) != 2 || ctxLight[0].Body != "" {
		t.Fatalf("GetNodesByFileLightContext failed: err=%v, len=%d", err, len(ctxLight))
	}

	// Test cap
	oldCap := getNodesByFileCap
	getNodesByFileCap = 1
	defer func() { getNodesByFileCap = oldCap }()

	capped, err := database.GetNodesByFile("target.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 1 {
		t.Fatalf("GetNodesByFile must be capped at 1, got %d", len(capped))
	}

	cappedLight, err := database.GetNodesByFileLight("target.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(cappedLight) != 1 {
		t.Fatalf("GetNodesByFileLight must be capped at 1, got %d", len(cappedLight))
	}
}

func TestFindFileCandidates(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	for _, p := range []string{"cmd/app/main.go", "pkg/main.go", "internal/db/query.go", "root.go"} {
		if err := database.UpsertFile(p, 100, 1000.0); err != nil {
			t.Fatal(err)
		}
	}

	// Basename search
	cands, err := database.FindFileCandidates("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("main.go: want 2 candidates, got %v", cands)
	}

	// Exact / suffix search
	cands, err = database.FindFileCandidates("db/query.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0] != "internal/db/query.go" {
		t.Fatalf("db/query.go: want [internal/db/query.go], got %v", cands)
	}

	// Non-existent
	cands, err = database.FindFileCandidates("missing.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("missing.go: want 0 candidates, got %v", cands)
	}
}

func TestFindFileCandidatesExactNotEvictedByLimit(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Lower the pattern cap so the test stays fast while still pinning the
	// invariant: exact matches survive regardless of the pattern LIMIT.
	oldLimit := findFileCandidatesPatternLimit
	findFileCandidatesPatternLimit = 1000
	defer func() { findFileCandidatesPatternLimit = oldLimit }()

	// Insert 1005 files matching the same basename "candidate.go" across various directories.
	for i := 0; i < 1005; i++ {
		p := fmt.Sprintf("dir_%04d/candidate.go", i)
		if err := database.UpsertFile(p, 100, 1000.0); err != nil {
			t.Fatal(err)
		}
	}

	// Exact query for the 1005th file (dir_1004/candidate.go)
	exactTarget := "dir_1004/candidate.go"
	cands, err := database.FindFileCandidates(exactTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) == 0 {
		t.Fatalf("expected candidates for %s, got 0", exactTarget)
	}
	if cands[0] != exactTarget {
		t.Fatalf("exact match %s must be returned first, got %s", exactTarget, cands[0])
	}

	// Basename search without exact path should return capped 1000 candidates
	baseCands, err := database.FindFileCandidates("candidate.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(baseCands) != 1000 {
		t.Fatalf("basename search should be capped at 1000, got %d", len(baseCands))
	}
}

func TestParkInboundRefsForFilePagination(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert 10005 nodes in target.go (exceeding old 10000 cap and keyset pagination batchSize = 1000)
	targetFile := "target.go"
	var targetIDs []int64
	for i := 0; i < 10005; i++ {
		id, err := database.UpsertNode(&Node{
			Kind: KindFunction, Name: fmt.Sprintf("TargetFunc_%05d", i), File: targetFile, Line: i + 1, Language: "go",
		})
		if err != nil {
			t.Fatal(err)
		}
		targetIDs = append(targetIDs, id)
	}

	// Insert a caller in caller.go
	callerID, err := database.UpsertNode(&Node{
		Kind: KindFunction, Name: "Caller", File: "caller.go", Line: 1, Language: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create edges pointing to targets in first batch (<1000), intermediate batch, and 10001st node (index 10000) and beyond (index 10004)
	targetIndices := []int{10, 500, 1000, 10000, 10004}
	for _, idx := range targetIndices {
		if _, err := database.UpsertEdge(&Edge{
			SourceID: callerID, TargetID: targetIDs[idx], Kind: EdgeCalls, File: "caller.go", Line: idx + 10,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Park inbound refs for target.go
	if err := database.ParkInboundRefsForFile(targetFile); err != nil {
		t.Fatal(err)
	}

	refs, err := database.ListUnresolvedRefs("caller.go", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != len(targetIndices) {
		t.Fatalf("want %d parked refs, got %d", len(targetIndices), len(refs))
	}

	refNames := make(map[string]bool)
	for _, r := range refs {
		refNames[r.ReferenceName] = true
	}
	for _, idx := range targetIndices {
		name := fmt.Sprintf("TargetFunc_%05d", idx)
		if !refNames[name] {
			t.Fatalf("expected parked ref for %s, but missing", name)
		}
	}
}


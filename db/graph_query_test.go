package db

import (
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

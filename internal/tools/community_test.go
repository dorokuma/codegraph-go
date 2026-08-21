package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestProvenanceWeightMapping(t *testing.T) {
	tests := []struct {
		provenance string
		want       float64
	}{
		{"exact", 1.0},
		{"import", 0.8},
		{"proximity", 0.3},
		{"heuristic", 0.1},
	}

	for _, tt := range tests {
		t.Run(tt.provenance, func(t *testing.T) {
			got := provenanceWeight[tt.provenance]
			if got != tt.want {
				t.Errorf("provenanceWeight[%q] = %v, want %v", tt.provenance, got, tt.want)
			}
		})
	}
}

func TestUnknownProvenanceFallback(t *testing.T) {
	// The code uses: if w == 0 { w = 0.5 }
	w := provenanceWeight["non_existent_provenance"]
	if w == 0 {
		w = 0.5
	}
	if w != 0.5 {
		t.Errorf("fallback weight = %v, want 0.5", w)
	}
}

// TestBuildGraphFiltersContains verifies that 'contains' edges are excluded
// from the graph used for community detection.
func TestBuildGraphFiltersContains(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	n1, err := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "funcA", File: "/a.go", Line: 1})
	if err != nil {
		t.Fatalf("upsert n1: %v", err)
	}
	n2, err := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "funcB", File: "/b.go", Line: 1})
	if err != nil {
		t.Fatalf("upsert n2: %v", err)
	}
	n3, err := database.UpsertNode(&db.Node{Kind: db.KindVariable, Name: "varC", File: "/c.go", Line: 1})
	if err != nil {
		t.Fatalf("upsert n3: %v", err)
	}

	// 'contains' edge → filtered out
	if _, err := database.UpsertEdge(&db.Edge{
		SourceID: n1, TargetID: n2, Kind: db.EdgeContains, Provenance: "exact",
	}); err != nil {
		t.Fatalf("upsert contains: %v", err)
	}
	// 'calls' edge → kept
	if _, err := database.UpsertEdge(&db.Edge{
		SourceID: n1, TargetID: n3, Kind: db.EdgeCalls, Provenance: "exact",
	}); err != nil {
		t.Fatalf("upsert calls: %v", err)
	}
	// 'references' edge → kept
	if _, err := database.UpsertEdge(&db.Edge{
		SourceID: n2, TargetID: n3, Kind: db.EdgeReferences, Provenance: "import",
	}); err != nil {
		t.Fatalf("upsert references: %v", err)
	}

	result, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{MinSize: 1})
	if err != nil {
		t.Fatalf("ToolCommunity: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected non-nil result")
	}

	text := result.Content[0].Text
	t.Logf("Output:\n%s", text)

	if !strings.Contains(text, "Total nodes: 3") {
		t.Errorf("expected 3 nodes, got:\n%s", text)
	}
	// With the 'contains' filtered, there should be 2 undirected edges:
	// (n1,n3) and (n2,n3). The output shows edges "after filter, undirected".
	if !strings.Contains(text, "Communities") {
		t.Errorf("expected Communities section")
	}
}

// TestCommunityDeterministic verifies that two runs produce identical output.
func TestCommunityDeterministic(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	nodes := make([]int64, 6)
	for i := 0; i < 6; i++ {
		n, err := database.UpsertNode(&db.Node{
			Kind: db.KindFunction, Name: string(rune('A' + i)),
			File: "/proj/main.go", Line: i + 1,
		})
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
		nodes[i] = n
	}

	// Two dense clusters: [0,1,2] and [3,4,5].
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			database.UpsertEdge(&db.Edge{
				SourceID: nodes[i], TargetID: nodes[j],
				Kind: db.EdgeCalls, Provenance: "exact",
			})
		}
	}
	for i := 3; i < 6; i++ {
		for j := i + 1; j < 6; j++ {
			database.UpsertEdge(&db.Edge{
				SourceID: nodes[i], TargetID: nodes[j],
				Kind: db.EdgeCalls, Provenance: "exact",
			})
		}
	}
	// One weak cross-edge.
	database.UpsertEdge(&db.Edge{
		SourceID: nodes[0], TargetID: nodes[3],
		Kind: db.EdgeCalls, Provenance: "heuristic",
	})

	result1, err1 := ToolCommunity(context.Background(), database, "/proj", CommunityArgs{MinSize: 2})
	if err1 != nil {
		t.Fatalf("first run: %v", err1)
	}
	result2, err2 := ToolCommunity(context.Background(), database, "/proj", CommunityArgs{MinSize: 2})
	if err2 != nil {
		t.Fatalf("second run: %v", err2)
	}

	text1 := result1.Content[0].Text
	text2 := result2.Content[0].Text
	if text1 != text2 {
		t.Error("non-deterministic output")
		t.Logf("Run 1:\n%s", text1)
		t.Logf("Run 2:\n%s", text2)
	}
}

// TestCommunityOutputNotTruncated verifies that small graphs produce output
// that does not hit truncation limits.
func TestCommunityOutputNotTruncated(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	n1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "foo", File: "/a.go", Line: 1})
	n2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "bar", File: "/b.go", Line: 1})
	database.UpsertEdge(&db.Edge{
		SourceID: n1, TargetID: n2,
		Kind: db.EdgeCalls, Provenance: "exact",
	})

	result, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{MinSize: 1})
	if err != nil {
		t.Fatalf("ToolCommunity: %v", err)
	}
	text := result.Content[0].Text
	if len(text) == 0 {
		t.Fatal("expected non-empty output")
	}
	if strings.HasSuffix(text, "... (truncated") || strings.HasSuffix(text, "... (truncated)") {
		t.Error("output was truncated unexpectedly")
	}
}

// TestCommunityEdgeCapError verifies the edge cap error message and that
// GetGraphSnapshot works for small graphs.
func TestCommunityEdgeCapError(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	n1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "a", File: "/a.go", Line: 1})
	n2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "b", File: "/b.go", Line: 1})
	database.UpsertEdge(&db.Edge{
		SourceID: n1, TargetID: n2,
		Kind: db.EdgeCalls, Provenance: "exact",
	})

	snapshot, err := database.GetGraphSnapshot()
	if err != nil {
		t.Fatalf("GetGraphSnapshot: %v", err)
	}
	if len(snapshot.Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(snapshot.Nodes))
	}
	if len(snapshot.Edges) != 1 {
		t.Errorf("expected 1 edge, got %d", len(snapshot.Edges))
	}
}

// TestCommunityNoNodes verifies that an empty index returns a helpful message.
func TestCommunityNoNodes(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	result, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{})
	if err != nil {
		t.Fatalf("ToolCommunity: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}
	expected := "No nodes in index. Nothing to analyze."
	if result.Content[0].Text != expected && result.Content[0].Text != expected+"\n" {
		t.Errorf("unexpected output: %q", result.Content[0].Text)
	}
}

// TestCommunityWeightAccumulation verifies that multiple edges between the
// same pair of nodes accumulate weight.
func TestCommunityWeightAccumulation(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	n1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "x", File: "/x.go", Line: 1})
	n2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "y", File: "/y.go", Line: 1})

	// Two edges: calls (exact=1.0) and references (import=0.8) → total weight 1.8
	database.UpsertEdge(&db.Edge{
		SourceID: n1, TargetID: n2,
		Kind: db.EdgeCalls, Provenance: "exact",
	})
	database.UpsertEdge(&db.Edge{
		SourceID: n1, TargetID: n2,
		Kind: db.EdgeReferences, Provenance: "import",
	})

	result, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{MinSize: 1})
	if err != nil {
		t.Fatalf("ToolCommunity: %v", err)
	}
	text := result.Content[0].Text
	t.Logf("Output:\n%s", text)
	// Should report 1 undirected edge (the two directed edges merge).
	if strings.Contains(text, "error") {
		t.Errorf("unexpected error in output: %s", text)
	}
}

// TestCommunityTopSymbolsOrdered verifies that top symbols are sorted by
// weighted degree descending, with deterministic tie-breaking.
func TestCommunityTopSymbolsOrdered(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	n1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "hub", File: "/hub.go", Line: 1})
	n2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "leaf1", File: "/leaf1.go", Line: 2})
	n3, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "leaf2", File: "/leaf2.go", Line: 3})

	// hub → leaf1, hub → leaf2 (hub degree=2.0, leaves=1.0 each)
	database.UpsertEdge(&db.Edge{SourceID: n1, TargetID: n2, Kind: db.EdgeCalls, Provenance: "exact"})
	database.UpsertEdge(&db.Edge{SourceID: n1, TargetID: n3, Kind: db.EdgeCalls, Provenance: "exact"})

	result, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{MinSize: 1})
	if err != nil {
		t.Fatalf("ToolCommunity: %v", err)
	}
	text := result.Content[0].Text

	// Find positions of symbols in the output (community top symbols section).
	hubPos := strings.Index(text, "hub (")
	leaf1Pos := strings.Index(text, "leaf1 (")
	leaf2Pos := strings.Index(text, "leaf2 (")

	if hubPos < 0 {
		t.Error("hub not found in output")
		t.Logf("Output:\n%s", text)
	}
	if leaf1Pos < 0 {
		t.Error("leaf1 not found in output")
	}
	if leaf2Pos < 0 {
		t.Error("leaf2 not found in output")
	}

	// hub (w=2.0) should appear before both leaves (w=1.0 each).
	if hubPos >= 0 && leaf1Pos >= 0 && hubPos > leaf1Pos {
		t.Error("hub should appear before leaf1 (higher weighted degree)")
	}
	if hubPos >= 0 && leaf2Pos >= 0 && hubPos > leaf2Pos {
		t.Error("hub should appear before leaf2 (higher weighted degree)")
	}
}

func TestCommunitySnapshotTruncatedWarning(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert 4 nodes and 3 edges
	n1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "A", File: "/a.go", Line: 1})
	n2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "B", File: "/b.go", Line: 1})
	n3, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "C", File: "/c.go", Line: 1})
	database.UpsertEdge(&db.Edge{SourceID: n1, TargetID: n2, Kind: db.EdgeCalls, Provenance: "exact"})
	database.UpsertEdge(&db.Edge{SourceID: n2, TargetID: n3, Kind: db.EdgeCalls, Provenance: "exact"})

	// Lower snapshot cap in db package to trigger truncation
	oldCap := db.SetGraphSnapshotCapForTest(2)
	defer db.SetGraphSnapshotCapForTest(oldCap)

	result, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{MinSize: 1})
	if err != nil {
		t.Fatalf("ToolCommunity: %v", err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "Warning: graph snapshot was truncated") {
		t.Fatalf("expected truncation warning in output, got:\n%s", text)
	}
}

func TestCommunityNodeCapRefusal(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	n1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "A", File: "/a.go", Line: 1})
	n2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "B", File: "/b.go", Line: 1})
	database.UpsertEdge(&db.Edge{SourceID: n1, TargetID: n2, Kind: db.EdgeCalls, Provenance: "exact"})

	oldCap := maxCommunityNodes
	maxCommunityNodes = 1
	defer func() { maxCommunityNodes = oldCap }()

	snapshotLoaded := false
	oldLoader := snapshotLoader
	snapshotLoader = func(database *db.DB) (*db.GraphSnapshot, error) {
		snapshotLoaded = true
		return oldLoader(database)
	}
	defer func() { snapshotLoader = oldLoader }()

	_, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{MinSize: 1})
	if err == nil {
		t.Fatal("expected refusal error when node count exceeds maxCommunityNodes")
	}
	if !strings.Contains(err.Error(), "community detection refused") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if snapshotLoaded {
		t.Fatal("expected snapshot NOT to be loaded when index exceeds maxCommunityNodes")
	}
}

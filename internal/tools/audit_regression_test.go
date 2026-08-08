package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// TestCommunityNodeCap: Louvain on an oversized graph must be refused with a
// clear error instead of burning CPU (audit high: full-graph communities had
// no timeout and no node cap). The cap is a var so the test can lower it.
func TestCommunityNodeCap(t *testing.T) {
	old := maxCommunityNodes
	maxCommunityNodes = 2
	defer func() { maxCommunityNodes = old }()

	database, cleanup := setupTestDB(t)
	defer cleanup()
	for i := 0; i < 3; i++ {
		if _, err := database.UpsertNode(&db.Node{
			Kind: db.KindFunction, Name: string(rune('a' + i)), File: "/a.go", Line: i + 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := ToolCommunity(context.Background(), database, "/workdir", CommunityArgs{})
	if err == nil {
		t.Fatal("expected refusal for graph over the node cap")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected refusal message, got: %v", err)
	}
}

// TestCommunityCanceledContext: a canceled request must abort before the
// (non-interruptible) Louvain pass.
func TestCommunityCanceledContext(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	n1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "a", File: "/a.go", Line: 1})
	n2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "b", File: "/b.go", Line: 1})
	database.UpsertEdge(&db.Edge{SourceID: n1, TargetID: n2, Kind: db.EdgeCalls, Provenance: "exact"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	if _, err := ToolCommunity(ctx, database, "/workdir", CommunityArgs{}); err == nil {
		t.Fatal("expected error for canceled context")
	}
}

// TestToolStatusNoAbsoluteDBPath: the status output must not echo the
// absolute DB path — only a workdir-relative or basename form (audit
// medium: status.go leaked database.Path()).
func TestToolStatusNoAbsoluteDBPath(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	result, err := ToolStatus(context.Background(), database, []string{"/workdir"}, "/workdir", StatusArgs{}, nil, 0)
	if err != nil {
		t.Fatalf("tool status: %v", err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "DB:") {
		t.Fatalf("expected DB line in status, got:\n%s", text)
	}
	if strings.Contains(text, database.Path()) {
		t.Fatalf("status must not leak the absolute DB path %q:\n%s", database.Path(), text)
	}
}

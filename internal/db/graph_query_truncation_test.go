package db

import (
	"fmt"
	"testing"
)

// TestGetNodesByFileBodiesLimitedTruncation pins the bodies variant: bodies
// are loaded AND the truncation flag is reported when the file holds more
// nodes than the limit.
func TestGetNodesByFileBodiesLimitedTruncation(t *testing.T) {
	database, _ := setupGraphDB(t)

	body := "func T() {}"
	for i := 1; i <= 3; i++ {
		if _, err := database.UpsertNode(&Node{
			Kind: KindFunction, Name: fmt.Sprintf("T%d", i), File: "cap.go", Line: i, Body: body, Language: "go",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Below the cap: truncated=true and bodies loaded.
	nodes, truncated, err := database.GetNodesByFileBodiesLimited("cap.go", 2)
	if err != nil || !truncated || len(nodes) != 2 {
		t.Fatalf("want 2 nodes with truncation=true, got %d nodes, truncated=%v, err=%v", len(nodes), truncated, err)
	}
	for _, n := range nodes {
		if n.Body != body {
			t.Fatalf("bodies must be loaded, got %q", n.Body)
		}
	}

	// Default cap (0): everything fits, no truncation.
	nodes, truncated, err = database.GetNodesByFileBodiesLimited("cap.go", 0)
	if err != nil || truncated || len(nodes) != 3 {
		t.Fatalf("default cap: want 3 nodes without truncation, got %d, truncated=%v, err=%v", len(nodes), truncated, err)
	}
}

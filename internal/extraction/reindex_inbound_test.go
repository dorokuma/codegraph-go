package extraction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func hasCall(t *testing.T, database *db.DB, caller, callee string) bool {
	t.Helper()
	cs, err := database.GetNodeByName(caller)
	if err != nil || len(cs) == 0 {
		return false
	}
	ts, err := database.GetNodeByName(callee)
	if err != nil || len(ts) == 0 {
		return false
	}
	calleeIDs := map[int64]bool{}
	for _, n := range ts {
		if n.Name == callee {
			calleeIDs[n.ID] = true
		}
	}
	for _, c := range cs {
		if c.Name != caller {
			continue
		}
		outs, err := database.GetOutgoingEdges(c.ID, []string{db.EdgeCalls})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range outs {
			if calleeIDs[e.TargetID] {
				return true
			}
		}
		ins, err := database.GetIncomingEdges(ts[0].ID, []string{db.EdgeCalls})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ins {
			if e.SourceID == c.ID {
				return true
			}
		}
	}
	return false
}

func TestReindexCalleeKeepsInboundCall(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	a := filepath.Join(root, "a.go")
	b := filepath.Join(root, "b.go")
	if err := os.WriteFile(a, []byte("package p\nfunc A() { B() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("package p\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orch := NewOrchestrator(database, root)
	orch.SetForceReindex(true)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}
	if !hasCall(t, database, "A", "B") {
		t.Fatal("setup: expected A→B calls edge after IndexAll")
	}

	if err := os.WriteFile(b, []byte("package p\nfunc B() { /* reindexed */ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.IndexFile(b); err != nil {
		t.Fatalf("IndexFile B: %v", err)
	}
	if !hasCall(t, database, "A", "B") {
		t.Fatal("A→B calls edge missing after reindexing B")
	}
}

func TestDeleteTreeClearsPrefix(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if err := os.MkdirAll(filepath.Join(root, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("package pkg\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "sub", "b.go"), []byte("package sub\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "root.go"), []byte("package p\nfunc Root() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orch := NewOrchestrator(database, root)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}
	if err := orch.DeleteTree(filepath.Join(root, "pkg")); err != nil {
		t.Fatal(err)
	}
	files, err := database.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "pkg/a.go" || f == "pkg/sub/b.go" {
			t.Fatalf("DeleteTree left %s: %v", f, files)
		}
	}
	foundRoot := false
	for _, f := range files {
		if f == "root.go" {
			foundRoot = true
		}
	}
	if !foundRoot {
		t.Fatalf("root.go should remain after DeleteTree(pkg): %v", files)
	}
	if nodes, _ := database.GetNodeByName("A"); len(nodes) != 0 {
		t.Fatalf("pkg nodes should be gone, got A=%+v", nodes)
	}
}

func TestIndexAllPrunesDeletedFile(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	keep := filepath.Join(root, "keep.go")
	gone := filepath.Join(root, "gone.go")
	if err := os.WriteFile(keep, []byte("package p\nfunc Keep() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gone, []byte("package p\nfunc Gone() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orch := NewOrchestrator(database, root)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}
	files, err := database.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "gone.go" {
			t.Fatalf("IndexAll left deleted file row: %v", files)
		}
	}
	if nodes, _ := database.GetNodeByName("Gone"); len(nodes) != 0 {
		t.Fatalf("Gone node should be pruned: %+v", nodes)
	}
	if nodes, _ := database.GetNodeByName("Keep"); len(nodes) == 0 {
		t.Fatal("Keep should still be indexed")
	}
}

package resolution_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
	"github.com/dorokuma/codegraph-go/extraction"
	"github.com/dorokuma/codegraph-go/resolution"
)

func indexDir(t *testing.T, dir string) *db.DB {
	t.Helper()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	orch := extraction.NewOrchestrator(database, dir)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}
	return database
}

func assertGraphCall(t *testing.T, database *db.DB, caller, callee string) {
	t.Helper()
	callers, err := database.GetNodeByName(caller)
	if err != nil || len(callers) == 0 {
		t.Fatalf("caller %s missing: %v", caller, err)
	}
	callees, err := database.GetCalleesWithKind(callers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range callees {
		if c.Name == callee && c.EdgeKind == db.EdgeCalls {
			// Ensure it came from the graph (we only wrote via resolution/extract).
			return
		}
	}
	t.Fatalf("%s callees should include %s via calls edge, got %+v", caller, callee, callees)
}

func TestParityGoCrossFileCall(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "testdata", "parity", "go"), dir)
	database := indexDir(t, dir)
	assertGraphCall(t, database, "Run", "Helper")
}

func TestParityTSCrossFileCall(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "testdata", "parity", "ts"), dir)
	database := indexDir(t, dir)
	assertGraphCall(t, database, "main", "greet")
}

func TestParityPyCrossFileCall(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "testdata", "parity", "py"), dir)
	database := indexDir(t, dir)
	assertGraphCall(t, database, "main", "greet")
}

func TestFailedRefRetriesWhenTargetAppears(t *testing.T) {
	dir := t.TempDir()
	// Only caller first — parks pending/failed for LateTarget.
	if err := os.WriteFile(filepath.Join(dir, "caller.go"), []byte(`package p
func Caller() { LateTarget() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	if _, err := orch.IndexFile(filepath.Join(dir, "caller.go")); err != nil {
		t.Fatal(err)
	}
	// Target missing → failed or still pending
	n, _ := database.CountUnresolvedRefs("")
	if n == 0 {
		// Might already be failed-marked with 0 pending — check failed
		failed, _ := database.CountUnresolvedRefs("failed")
		if failed == 0 {
			t.Fatal("expected unresolved ref while target missing")
		}
	}

	// Target appears
	target := filepath.Join(dir, "target.go")
	if err := os.WriteFile(target, []byte(`package p
func LateTarget() {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.IndexFile(target); err != nil {
		t.Fatal(err)
	}
	// ResolveForFiles on IndexFile should retry
	assertGraphCall(t, database, "Caller", "LateTarget")
}

func TestResolveAllIdempotent(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "testdata", "parity", "go"), dir)
	database := indexDir(t, dir)
	st, err := resolution.ResolveAll(database, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Resolved != 0 {
		t.Fatalf("second ResolveAll should be idle, got %+v", st)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

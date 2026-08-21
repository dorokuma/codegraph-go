package resolution_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
	"github.com/dorokuma/codegraph-go/internal/resolution"
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
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "go"), dir)
	database := indexDir(t, dir)
	assertGraphCall(t, database, "Run", "Helper")
}

func TestParityTSCrossFileCall(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "ts"), dir)
	database := indexDir(t, dir)
	assertGraphCall(t, database, "main", "greet")
}

func TestParityPyCrossFileCall(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "py"), dir)
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
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "go"), dir)
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

// TestProvHeuristicReject verifies M-8: same-package cross-file calls resolve
// via ProvProximity, while cross-package calls without import closure are rejected.
func TestProvHeuristicReject(t *testing.T) {
	// Part 1: Same-package cross-file resolution (ProvProximity).
	dir1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir1, "main.go"), []byte(`package p
func Caller() string { return Helper() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "helper.go"), []byte(`package p
func Helper() string { return "ok" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db1 := indexDir(t, dir1)
	assertGraphCall(t, db1, "Caller", "Helper")

	// Part 2: Cross-package call without import closure should NOT resolve.
	// Place the caller in a subdirectory so sibling-directory heuristic doesn't fire.
	dir2 := t.TempDir()
	callerDir := filepath.Join(dir2, "caller")
	if err := os.MkdirAll(callerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(callerDir, "root.go"), []byte(`package root
func RootCaller() string { return pkg.Foo() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(dir2, "lib", "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "pkg.go"), []byte(`package pkg
func Foo() string { return "ok" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db2 := indexDir(t, dir2)
	// RootCaller -> Foo: cross-package, no go.mod, should NOT resolve.
	callers, err := db2.GetNodeByName("RootCaller")
	if err != nil || len(callers) == 0 {
		t.Fatalf("RootCaller missing: %v", err)
	}
	callees, err := db2.GetCalleesWithKind(callers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range callees {
		if c.Name == "Foo" && c.EdgeKind == db.EdgeCalls {
			t.Fatal("RootCaller -> Foo should NOT resolve cross-package without import closure")
		}
	}

	// Part 3: Same-package cross-file in a subdirectory also works.
	dir3 := t.TempDir()
	sub := filepath.Join(dir3, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte(`package sub
func Alpha() string { return Beta() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.go"), []byte(`package sub
func Beta() string { return "ok" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db3 := indexDir(t, dir3)
	assertGraphCall(t, db3, "Alpha", "Beta")
}

// TestResolveForFilesChangedNamesBranch verifies F1: ResolveForFiles must
// retry refs whose name is defined by a changed file (the changedNames branch),
// even when the ref itself lives in a different, unchanged file. The name
// filter is pushed down to SQL (ListUnresolvedRefsByNames), so this exercises
// the exact-equality match path end to end.
func TestResolveForFilesChangedNamesBranch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "caller.go"), []byte(`package p
func Caller() { Missing() }
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
	// Ref parked while Missing is undefined.
	n, _ := database.CountUnresolvedRefs("")
	if n == 0 {
		t.Fatal("expected a parked unresolved ref while target missing")
	}

	// Target appears in another file. Insert its node directly (no IndexFile,
	// which would itself trigger ResolveForFiles) so only the changedNames
	// branch of the direct call below can resolve the ref.
	if _, err := database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "Missing", File: "target.go", Line: 1, Language: "go",
	}); err != nil {
		t.Fatal(err)
	}

	st, err := resolution.ResolveForFiles(database, dir, []string{"target.go"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resolved != 1 {
		t.Fatalf("expected 1 resolved via changedNames branch, got %+v", st)
	}
	if st.Retried < 1 {
		t.Fatalf("expected the failed ref to be counted as retried, got %+v", st)
	}
	assertGraphCall(t, database, "Caller", "Missing")
}

// TestResolveForFilesEmptyTailChangedNames verifies S2: a row with
// name_tail=” and reference_name holding the full qualified name ("pkg.Foo")
// must be retried when a changed file defines the bare tail symbol ("Foo"),
// matching the pre-F1 Go scan. The SQL pushdown alone cannot see this row
// (reference_name "pkg.Foo" ∉ names, name_tail ” ∉ names); the empty-tail
// supplement in ResolveForFiles re-applies nameTail matching in Go.
func TestResolveForFilesEmptyTailChangedNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "caller.go"), []byte(`package p
func Caller() { _ = 1 }
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
	callerKey := db.StoragePath(dir, filepath.Join(dir, "caller.go"))
	callers, err := database.GetNodesByFile(callerKey)
	if err != nil || len(callers) == 0 {
		t.Fatalf("caller nodes missing: %v", err)
	}
	var callerID int64
	for _, n := range callers {
		if n.Name == "Caller" {
			callerID = n.ID
		}
	}
	if callerID == 0 {
		t.Fatalf("Caller node missing in %+v", callers)
	}

	// Historical/anomalous row: full qualified reference name, empty tail.
	if _, err := database.InsertUnresolvedRef(&db.UnresolvedRef{
		FromNode:      callerID,
		ReferenceName: "pkg.Foo",
		ReferenceKind: db.EdgeCalls,
		Line:          2,
		FilePath:      callerKey,
		Language:      "go",
		Status:        "pending",
		NameTail:      "",
	}); err != nil {
		t.Fatal(err)
	}

	// Changed file defines the bare tail symbol only.
	if _, err := database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "Foo", File: "target.go", Line: 1, Language: "go",
	}); err != nil {
		t.Fatal(err)
	}

	st, err := resolution.ResolveForFiles(database, dir, []string{"target.go"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resolved != 1 {
		t.Fatalf("expected the empty-tail ref to resolve via changedNames branch, got %+v", st)
	}
	if n, _ := database.CountUnresolvedRefs(""); n != 0 {
		t.Fatalf("expected the ref row to be deleted after resolution, got %d remaining", n)
	}
	assertGraphCall(t, database, "Caller", "Foo")
}

// TestNameResolution10001SameNameCandidates tests that CollectCandidates and
// resolver.ResolveAll / MatchName correctly handle and resolve when there are
// 10,001 candidates with the exact same name across different files, ensuring
// candidate #10,001 (such as the same-file definition) is not silently dropped by pagination/caps.
func TestNameResolution10001SameNameCandidates(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Insert 10,001 nodes named "CommonService"
	const total = 10001
	for i := 0; i < total; i++ {
		file := filepath.Join(dir, fmt.Sprintf("service_%05d.go", i))
		if _, err := database.UpsertNode(&db.Node{
			Kind:     db.KindFunction,
			Name:     "CommonService",
			File:     file,
			Line:     1,
			Language: "go",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The 10,001st node is in service_10000.go
	targetFile := filepath.Join(dir, fmt.Sprintf("service_%05d.go", total-1))

	// In the same file (service_10000.go), insert a caller node
	callerID, err := database.UpsertNode(&db.Node{
		Kind:     db.KindFunction,
		Name:     "Client",
		File:     targetFile,
		Line:     10,
		Language: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Insert an unresolved ref from Client to CommonService in service_10000.go
	if _, err := database.InsertUnresolvedRef(&db.UnresolvedRef{
		FromNode:      callerID,
		ReferenceName: "CommonService",
		ReferenceKind: db.EdgeCalls,
		Line:          11,
		FilePath:      targetFile,
		Language:      "go",
		Status:        "pending",
		NameTail:      "CommonService",
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Verify CollectCandidates returns all 10,001 candidates
	cands, err := resolution.CollectCandidates(database, "CommonService")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != total {
		t.Fatalf("CollectCandidates returned %d candidates, want %d", len(cands), total)
	}

	// 2. Resolve the pending reference end-to-end
	st, err := resolution.ResolveAll(database, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Resolved != 1 {
		t.Fatalf("expected 1 resolved reference, got %+v", st)
	}

	// Verify caller in service_10000.go resolved to the 10001st candidate in service_10000.go
	callees, err := database.GetCalleesWithKind(callerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(callees) != 1 {
		t.Fatalf("expected 1 callee, got %d: %+v", len(callees), callees)
	}
	if callees[0].Name != "CommonService" || callees[0].File != targetFile {
		t.Fatalf("expected call to CommonService in %s, got %+v", targetFile, callees[0])
	}
}


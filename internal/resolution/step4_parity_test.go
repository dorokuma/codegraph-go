package resolution_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
	"github.com/dorokuma/codegraph-go/internal/resolution"
)

// TestParityAliasAtSlash: @/lib/utils style import resolves and call edge is graph-linked.
func TestParityAliasAtSlash(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "alias"), dir)
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}

	from := filepath.Join(dir, "src", "main.ts")
	files := resolution.ResolveImportPath(dir, from, "@/lib/utils", "typescript")
	want := filepath.Join(dir, "src", "lib", "utils.ts")
	if !containsPath(files, want) {
		t.Fatalf("@/lib/utils did not resolve to %s: %v", want, files)
	}

	assertGraphCall(t, database, "run", "formatName")
}

// TestParityGoModulePath: go.mod module path + replace drive import resolution.
func TestParityGoModulePath(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "gomod"), dir)
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}

	from := filepath.Join(dir, "main.go")
	aFiles := resolution.ResolveImportPath(dir, from, "example.com/demo/pkga", "go")
	if !containsPath(aFiles, filepath.Join(dir, "pkga", "a.go")) {
		t.Fatalf("module path resolve: %v", aFiles)
	}
	bFiles := resolution.ResolveImportPath(dir, from, "example.com/replaced", "go")
	if !containsPath(bFiles, filepath.Join(dir, "pkgb", "b.go")) {
		t.Fatalf("replace path resolve: %v", bFiles)
	}

	assertGraphCall(t, database, "Run", "Helper")
	assertGraphCall(t, database, "Run", "Replaced")
}

// TestProjectPathIsolation: querying subproject A must not see symbols only in B's index.
func TestProjectPathIsolation(t *testing.T) {
	base := t.TempDir()
	projA := filepath.Join(base, "A")
	projB := filepath.Join(base, "B")
	os.MkdirAll(projA, 0o755)
	os.MkdirAll(projB, 0o755)

	os.WriteFile(filepath.Join(projA, "a.go"), []byte("package a\nfunc OnlyInA() {}\n"), 0o644)
	os.WriteFile(filepath.Join(projB, "b.go"), []byte("package b\nfunc OnlyInB() {}\n"), 0o644)

	dbA, err := db.Open(projA)
	if err != nil {
		t.Fatal(err)
	}
	defer dbA.Close()
	dbB, err := db.Open(projB)
	if err != nil {
		t.Fatal(err)
	}
	defer dbB.Close()

	if _, _, err := extraction.NewOrchestrator(dbA, projA).IndexAll(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := extraction.NewOrchestrator(dbB, projB).IndexAll(); err != nil {
		t.Fatal(err)
	}

	if got := db.FindNearestCodeGraphRoot(projA); got != projA {
		t.Fatalf("nearest A = %q", got)
	}
	if got := db.FindNearestCodeGraphRoot(projB); got != projB {
		t.Fatalf("nearest B = %q", got)
	}

	aNodes, _ := dbA.GetNodeByName("OnlyInA")
	bLeak, _ := dbA.GetNodeByName("OnlyInB")
	if len(aNodes) == 0 {
		t.Fatal("OnlyInA missing from A's index")
	}
	if len(bLeak) != 0 {
		t.Fatalf("OnlyInB leaked into A's index: %+v", bLeak)
	}
	bNodes, _ := dbB.GetNodeByName("OnlyInB")
	aLeak, _ := dbB.GetNodeByName("OnlyInA")
	if len(bNodes) == 0 {
		t.Fatal("OnlyInB missing from B's index")
	}
	if len(aLeak) != 0 {
		t.Fatalf("OnlyInA leaked into B's index: %+v", aLeak)
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

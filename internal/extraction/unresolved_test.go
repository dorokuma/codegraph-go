package extraction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestIndexThenResolveCrossFileCalls(t *testing.T) {
	dir := t.TempDir()
	// a.go defines Callee; b.go calls it — extract parks, ResolveAll links.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(`package p
func Callee() {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(`package p
func Caller() {
	Callee()
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module t\n"), 0o644)

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, dir)
	files, nodes, err := orch.IndexAll()
	if err != nil {
		t.Fatal(err)
	}
	if files < 2 || nodes < 2 {
		t.Fatalf("index files=%d nodes=%d", files, nodes)
	}

	// After IndexAll (+ ResolveAll), cross-file call is a live graph edge.
	cs, err := database.GetNodeByName("Caller")
	if err != nil || len(cs) == 0 {
		t.Fatal("Caller missing")
	}
	callees, err := database.GetCallees(cs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	foundCallee := false
	for _, c := range callees {
		if c.Name == "Callee" {
			foundCallee = true
		}
	}
	if !foundCallee {
		t.Fatalf("cross-file Caller→Callee edge missing after resolve: %+v", callees)
	}

	// Same-file call should still be a live edge.
	same := filepath.Join(dir, "same.go")
	if err := os.WriteFile(same, []byte(`package p
func LocalA() {}
func LocalB() { LocalA() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.IndexFile(same); err != nil {
		t.Fatal(err)
	}
	bs, err := database.GetNodeByName("LocalB")
	if err != nil || len(bs) == 0 {
		t.Fatalf("LocalB missing: %v", err)
	}
	callees, err = database.GetCallees(bs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range callees {
		if c.Name == "LocalA" {
			found = true
		}
	}
	if !found {
		t.Fatalf("same-file LocalB→LocalA edge missing: %+v", callees)
	}
}

func TestReindexDoesNotLeaveZombieUnresolved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	src1 := `package p
func A() {}
func B() { MissingOne() }
`
	src2 := `package p
func A() {}
func B() { MissingTwo() }
`
	if err := os.WriteFile(path, []byte(src1), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module t\n"), 0o644)

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := NewOrchestrator(database, dir)
	if _, err := orch.IndexFile(path); err != nil {
		t.Fatal(err)
	}
	n1, _ := database.CountUnresolvedRefs("")
	if n1 == 0 {
		t.Fatal("expected unresolved refs after first index (pending or failed)")
	}

	// Change the call target and reindex — old MissingOne must not linger.
	if err := os.WriteFile(path, []byte(src2), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.IndexFile(path); err != nil {
		t.Fatal(err)
	}
	n2, err := database.CountUnresolvedRefs("")
	if err != nil {
		t.Fatal(err)
	}
	if n2 == 0 {
		t.Fatal("expected unresolved refs after reindex")
	}

	// Inspect rows: only MissingTwo, no MissingOne.
	refs, err := database.ListUnresolvedRefs(path, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range refs {
		names = append(names, r.ReferenceName)
		if r.ReferenceName == "MissingOne" {
			t.Fatalf("zombie unresolved ref MissingOne still present: %v", names)
		}
	}
	foundTwo := false
	for _, name := range names {
		if name == "MissingTwo" {
			foundTwo = true
		}
	}
	if !foundTwo {
		t.Fatalf("expected MissingTwo pending, got %v", names)
	}
}

func TestNameTail(t *testing.T) {
	cases := map[string]string{
		"greet":      "greet",
		"util.greet": "greet",
		"a.b.c":      "c",
		"Ctrl@act":   "act",
		"":           "",
	}
	for in, want := range cases {
		if got := NameTail(in); got != want {
			t.Errorf("NameTail(%q)=%q want %q", in, got, want)
		}
	}
}

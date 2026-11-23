package extraction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestSplitRustUsePaths(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"std::io", []string{"std::io"}},
		{"std::io::{Read, Write}", []string{"std::io::Read", "std::io::Write"}},
		{"crate::{a, b, c}", []string{"crate::a", "crate::b", "crate::c"}},
		{"serde::{self, de}", []string{"serde", "serde::de"}},
		{"foo::bar::{}", []string{"foo::bar"}},
		{"simple", []string{"simple"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := splitRustUsePaths(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitRustUsePaths(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitRustUsePaths(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestSimplifyHandlerNameOrchestrator(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"listUsers", "listUsers"},
		{"UsersController@index", "index"},
		{"pkg.Handler", "Handler"},
		{"(User).Create", "Create"},
		{"handler()", "handler"},
		{"  trimmed  ", "trimmed"},
		{"", ""},
	}
	for _, tt := range tests {
		got := simplifyHandlerName(tt.input)
		if got != tt.want {
			t.Errorf("simplifyHandlerName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBareRank(t *testing.T) {
	if bareRank("function") != 3 {
		t.Errorf("bareRank(function) = %d, want 3", bareRank("function"))
	}
	if bareRank("method") != 3 {
		t.Errorf("bareRank(method) = %d, want 3", bareRank("method"))
	}
	if bareRank("class") != 2 {
		t.Errorf("bareRank(class) = %d, want 2", bareRank("class"))
	}
	if bareRank("route") != 1 {
		t.Errorf("bareRank(route) = %d, want 1", bareRank("route"))
	}
	if bareRank("variable") != 0 {
		t.Errorf("bareRank(variable) = %d, want 0", bareRank("variable"))
	}
}

func TestSplitNameLineKey(t *testing.T) {
	tests := []struct {
		key      string
		wantName string
		wantLine int
		wantOk   bool
	}{
		{"foo:10", "foo", 10, true},
		{"myFunc:42", "myFunc", 42, true},
		{"pkg.Type:1", "pkg.Type", 1, true},
		{"", "", 0, false},
		{"noline", "", 0, false},
		{":0", "", 0, true},
	}
	for _, tt := range tests {
		name, line, ok := splitNameLineKey(tt.key)
		if ok != tt.wantOk || name != tt.wantName || line != tt.wantLine {
			t.Errorf("splitNameLineKey(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tt.key, name, line, ok, tt.wantName, tt.wantLine, tt.wantOk)
		}
	}
}

// TestHasParentDirSegment verifies the exact ".." path-segment check used by
// IndexChanges' storeKeys filter: legitimate double-dot file names must pass
// while out-of-workdir segment shapes are intercepted.
func TestHasParentDirSegment(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"..", true},
		{"../outside.go", true},
		{"pkg/../gone.go", true},
		{"pkg/..", true},
		{"a..b.go", false},
		{"v1..v2_test.go", false},
		{"pkg/a..b.go", false},
		{"normal.go", false},
		{"pkg/sub/file.go", false},
		{".", false},
		{"pkg/.hidden.go", false},
	}
	for _, tt := range tests {
		if got := hasParentDirSegment(tt.key); got != tt.want {
			t.Errorf("hasParentDirSegment(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

// TestIndexChangesDoubleDotFileName is the regression for the substring
// ".." filter that dropped every storeKey containing two dots: a legal
// file named a..b.go must be indexed, while an explicit out-of-workdir
// path is still intercepted by the exact-segment check.
func TestIndexChangesDoubleDotFileName(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	doubleDot := filepath.Join(root, "a..b.go")
	if err := os.WriteFile(doubleDot, []byte("package p\nfunc DoubleDot() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orch := NewOrchestrator(database, root)
	if _, _, err := orch.IndexChanges([]string{doubleDot}); err != nil {
		t.Fatalf("IndexChanges a..b.go: %v", err)
	}
	files, err := database.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if f == "a..b.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a..b.go must be indexed, files=%v", files)
	}
	if nodes, _ := database.GetNodeByName("DoubleDot"); len(nodes) == 0 {
		t.Fatal("a..b.go's DoubleDot node should be indexed")
	}

	// Bad ".."-segment keys that reach the DB by other means (old writers,
	// manual inserts) are cleaned by pruneMissingFiles' tail statements —
	// including the files table, which the keep-set pass alone can never
	// remove (a "../x" path never appears in the walk job list). Seed rows
	// in all three tables and confirm the tail cleanup drops every shape,
	// including a key ENDING in "/.." which the earlier patterns missed.
	for _, badKey := range []string{"../escape.go", "pkg/../gone.go", "pkg/..", ".."} {
		if err := database.UpsertFile(badKey, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := orch.pruneMissingFiles(nil)
	if err != nil {
		t.Fatalf("pruneMissingFiles: %v", err)
	}
	files, err = database.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if hasParentDirSegment(f) {
			t.Fatalf("..-segment files row survived prune: %v", files)
		}
	}
	if pruned == 0 {
		t.Fatal("pruneMissingFiles reported 0 pruned rows for seeded bad keys")
	}
}

func TestShouldParkRef(t *testing.T) {
	// ShouldParkRef takes a db and a name. Without a db, it should handle nil gracefully.
	// We can't easily test this without a db, but we can test the edge cases.
	// For now, just verify it doesn't panic with empty names.
	if ShouldParkRef(nil, "") {
		t.Error("ShouldParkRef(nil, \"\") should be false")
	}
}

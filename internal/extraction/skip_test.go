package extraction

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShouldSkipDir(t *testing.T) {
	cases := []struct {
		path, name string
		skip       bool
	}{
		{"/home/u/node_modules", "node_modules", true},
		{"/proj/pkg", "pkg", false},
		{"/home/u/go/pkg/mod/github.com/x", "x", true},
		{"/home/u/myapp/mod", "mod", false},
		{"/root/go", "go", true},
		{"/root/go/pkg", "pkg", true},
		{"/root/go/bin", "bin", true},
		{"/root/go/pkg/mod/foo", "foo", true},
		{"/root/codegraph-go", "codegraph-go", false},
		{"/root/codegraph-go/pkg", "pkg", false},
		{"/root/codegraph-go/pkg/mod", "mod", false},
		{"/home/u/codegraph-go/extraction", "extraction", false},
		{"/root/.cargo/registry/src", "src", true},
		{"/root/code_references/x", "x", true},
	}
	for _, c := range cases {
		got := ShouldSkipDir(c.path, c.name)
		if got != c.skip {
			t.Fatalf("ShouldSkipDir(%q, %q)=%v want %v", c.path, c.name, got, c.skip)
		}
	}
}

func TestHomeModeTopLevel(t *testing.T) {
	home := t.TempDir()
	// Fake broad workdir by using a real dir; IsBroadWorkdir checks UserHomeDir/ /root.
	// We test ShouldSkipDirIn logic via a manual scenario: workdir with project + junk.
	// Override: call with workdir that IsBroadWorkdir may not recognize — so unit-test
	// HasProjectMarker + relative top-level filter by temporarily setting HOME.

	oldHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", oldHome) })
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}

	proj := filepath.Join(home, "myapp")
	junk := filepath.Join(home, "random-dump")
	hidden := filepath.Join(home, ".something")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	// nested inside project should not be home-filtered
	nested := filepath.Join(proj, "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	if !IsBroadWorkdir(home) {
		t.Fatalf("expected home %s to be broad", home)
	}
	if ShouldSkipDirIn(home, proj, "myapp") {
		t.Fatal("project top-level should index")
	}
	if !ShouldSkipDirIn(home, junk, "random-dump") {
		t.Fatal("non-project top-level should skip in home mode")
	}
	if !ShouldSkipDirIn(home, hidden, ".something") {
		t.Fatal("hidden top-level should skip in home mode")
	}
	if ShouldSkipDirIn(home, nested, "internal") {
		t.Fatal("nested under project should not be home-filtered")
	}

	// HOME_INDEX_ALL opts out
	t.Setenv("CODEGRAPH_GO_HOME_INDEX_ALL", "1")
	if ShouldSkipDirIn(home, junk, "random-dump") {
		t.Fatal("HOME_INDEX_ALL should allow non-project top-level")
	}
}

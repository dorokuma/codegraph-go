package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestFindNearestCodeGraphRoot(t *testing.T) {
	base := t.TempDir()
	// parent has no index
	// child A has .codegraph
	// child B has nested deeper .codegraph
	a := filepath.Join(base, "projA")
	b := filepath.Join(base, "projB", "nested")
	os.MkdirAll(filepath.Join(a, ".codegraph"), 0o755)
	os.MkdirAll(filepath.Join(b, ".codegraph"), 0o755)
	os.MkdirAll(filepath.Join(base, "projB", "nested", "pkg"), 0o755)

	if got := db.FindNearestCodeGraphRoot(filepath.Join(a, "src")); got != a {
		t.Fatalf("from A/src got %q want %q", got, a)
	}
	if got := db.FindNearestCodeGraphRoot(filepath.Join(b, "pkg")); got != b {
		t.Fatalf("from B/nested/pkg got %q want %q", got, b)
	}
	// sibling without index → empty (must NOT climb into A)
	empty := filepath.Join(base, "projC")
	os.MkdirAll(empty, 0o755)
	if got := db.FindNearestCodeGraphRoot(empty); got != "" {
		t.Fatalf("unindexed sibling should be empty, got %q", got)
	}
	if !db.IsIndexed(a) {
		t.Fatal("IsIndexed(A)")
	}
	if db.IsIndexed(empty) {
		t.Fatal("IsIndexed(empty) should be false")
	}
}

// TestFindNearestCodeGraphRootBounded: when a candidate root's own skip
// rules exclude an intermediate directory between the root and startPath
// (e.g. the .wt worktree container or an exclude_dirs rule), the bounded
// variant must return "" instead of adopting that blind index — and must
// not keep climbing to a higher ancestor.
func TestFindNearestCodeGraphRootBounded(t *testing.T) {
	base := t.TempDir()
	// Workspace root holds an index; a worktree lives under its .wt
	// container which the workspace's own index excludes.
	wt := filepath.Join(base, ".wt", "repo", "feat-x")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}

	skipWt := func(ancestor, childPath, childName string) bool {
		return ancestor == base && childName == ".wt"
	}
	if got := db.FindNearestCodeGraphRootBounded(wt, skipWt); got != "" {
		t.Fatalf("worktree inside excluded .wt must resolve to '', got %q", got)
	}
	// Same behaviour through an exclude_dirs-style rule.
	skipRef := func(ancestor, childPath, childName string) bool {
		return childName == "repo"
	}
	if got := db.FindNearestCodeGraphRootBounded(wt, skipRef); got != "" {
		t.Fatalf("worktree under excluded 'repo' segment must resolve to '', got %q", got)
	}

	// Unrelated subtree: nothing skipped → the indexed root still resolves.
	proj := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := db.FindNearestCodeGraphRootBounded(filepath.Join(proj, "src"), skipWt); got != base {
		t.Fatalf("unexcluded path must still adopt the root, got %q want %q", got, base)
	}
	// nil predicate = the old unbounded behaviour.
	if got := db.FindNearestCodeGraphRootBounded(wt, nil); got != base {
		t.Fatalf("nil predicate must keep unbounded semantics, got %q want %q", got, base)
	}
	// A start path that IS the indexed root never consults the predicate.
	if got := db.FindNearestCodeGraphRootBounded(base, skipWt); got != base {
		t.Fatalf("root itself must resolve to itself, got %q", got)
	}
}

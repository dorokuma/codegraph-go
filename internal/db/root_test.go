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

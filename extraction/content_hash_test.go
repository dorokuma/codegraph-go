package extraction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestContentHashSkipUnchanged(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	src := filepath.Join(root, "a.go")
	body := []byte("package p\nfunc Hello() {}\n")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}

	orch := NewOrchestrator(database, root)
	n1, err := orch.IndexFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if n1 == 0 {
		t.Fatal("expected nodes on first index")
	}
	hash, err := database.GetFileContentHash(src)
	if err != nil || hash == "" {
		t.Fatalf("hash after index: %q err=%v", hash, err)
	}

	// Touch mtime without changing bytes — IndexFile should content-hash short-circuit.
	// (force is false by default)
	info1, _ := os.Stat(src)
	// rewrite same bytes to bump mtime
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(src)
	if info2.ModTime().Equal(info1.ModTime()) && info2.Size() == info1.Size() {
		// still ok — indexIfNeeded may skip entirely via mtime; force path below
	}

	n2, err := orch.indexFile(src, "go")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("unchanged content should skip extract, got nodes=%d", n2)
	}
	hash2, _ := database.GetFileContentHash(src)
	if hash2 != hash {
		t.Fatalf("hash changed on skip %q → %q", hash, hash2)
	}

	// Real edit must reindex
	if err := os.WriteFile(src, []byte("package p\nfunc Hello() {}\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n3, err := orch.indexFile(src, "go")
	if err != nil {
		t.Fatal(err)
	}
	if n3 == 0 {
		t.Fatal("edited file should reindex")
	}
	hash3, _ := database.GetFileContentHash(src)
	if hash3 == hash {
		t.Fatal("hash should change after edit")
	}
}

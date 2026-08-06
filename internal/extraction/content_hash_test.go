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
	key := db.StoragePath(root, src)
	hash, err := database.GetFileContentHash(key)
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

	n2, err := orch.indexFile(src, "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("unchanged content should skip extract, got nodes=%d", n2)
	}
	hash2, _ := database.GetFileContentHash(key)
	if hash2 != hash {
		t.Fatalf("hash changed on skip %q → %q", hash, hash2)
	}

	// Real edit must reindex
	if err := os.WriteFile(src, []byte("package p\nfunc Hello() {}\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n3, err := orch.indexFile(src, "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n3 == 0 {
		t.Fatal("edited file should reindex")
	}
	hash3, _ := database.GetFileContentHash(key)
	if hash3 == hash {
		t.Fatal("hash should change after edit")
	}
}

// TestIndexIfNeededAndIndexFileConsistent verifies F4: the two entry paths
// (indexIfNeeded with its metadata/content-hash gate, and a direct indexFile)
// must produce identical DB state for identical bytes, and indexIfNeeded must
// skip cleanly when nothing changed. The bytes read for the hash gate are the
// same bytes indexFile indexes (no second read), so stored hashes always match
// the indexed content.
func TestIndexIfNeededAndIndexFileConsistent(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	src := filepath.Join(root, "a.go")
	bodyA := []byte("package p\nfunc Hello() {}\n")
	if err := os.WriteFile(src, bodyA, 0o644); err != nil {
		t.Fatal(err)
	}
	orch := NewOrchestrator(database, root)
	key := db.StoragePath(root, src)

	// Path 1: indexIfNeeded (metadata unchanged → hash gate → reindex with
	// the already-read bytes).
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	files, nodes1, err := orch.indexIfNeeded(src, info, "go")
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || nodes1 == 0 {
		t.Fatalf("indexIfNeeded: files=%d nodes=%d", files, nodes1)
	}
	hash1, err := database.GetFileContentHash(key)
	if err != nil || hash1 == "" {
		t.Fatalf("hash after indexIfNeeded: %q err=%v", hash1, err)
	}
	if hash1 != hashContent(bodyA) {
		t.Fatalf("stored hash must match indexed bytes: %q vs %q", hash1, hashContent(bodyA))
	}
	// indexFile reports extracted symbols; the DB also holds the file node.
	if dbNodes := nodesInDB(t, database, key); dbNodes != nodes1+1 {
		t.Fatalf("indexIfNeeded reported %d symbols but DB has %d nodes", nodes1, dbNodes)
	}

	// Path 2: direct indexFile with changed content.
	bodyB := []byte("package p\nfunc Hello() {}\nfunc World() {}\n")
	if err := os.WriteFile(src, bodyB, 0o644); err != nil {
		t.Fatal(err)
	}
	nodes2, err := orch.indexFile(src, "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if nodes2 == 0 {
		t.Fatal("indexFile must index changed content")
	}
	hash2, err := database.GetFileContentHash(key)
	if err != nil || hash2 == "" {
		t.Fatalf("hash after indexFile: %q err=%v", hash2, err)
	}
	if hash2 != hashContent(bodyB) {
		t.Fatalf("hash after indexFile must match new bytes: %q vs %q", hash2, hashContent(bodyB))
	}
	if dbNodes := nodesInDB(t, database, key); dbNodes != nodes2+1 {
		t.Fatalf("indexFile reported %d symbols but DB has %d nodes", nodes2, dbNodes)
	}

	// Path 3: indexIfNeeded on the unchanged file must short-circuit (0,0,nil).
	info2, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	files3, nodes3, err := orch.indexIfNeeded(src, info2, "go")
	if err != nil {
		t.Fatal(err)
	}
	if files3 != 0 || nodes3 != 0 {
		t.Fatalf("unchanged file must skip: files=%d nodes=%d", files3, nodes3)
	}
	hash3, _ := database.GetFileContentHash(key)
	if hash3 != hash2 {
		t.Fatalf("skip must not change stored hash: %q → %q", hash2, hash3)
	}
}

func nodesInDB(t *testing.T, database *db.DB, key string) int {
	t.Helper()
	ns, err := database.GetNodesByFile(key)
	if err != nil {
		t.Fatal(err)
	}
	return len(ns)
}

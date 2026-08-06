package extraction

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// ---- A4: parse failure keeps old index; successful empty extract clears it ----

func TestIndexFileParseFailureKeepsOldIndex(t *testing.T) {
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
	if _, err := orch.IndexFile(src); err != nil {
		t.Fatal(err)
	}
	hs, _ := database.GetNodeByName("Hello")
	if len(hs) != 1 {
		t.Fatalf("setup: expected Hello indexed, got %d", len(hs))
	}

	// Change content (passes the content-hash gate), then make every extractor
	// fail — the orchestrator must NOT clear the old symbols.
	broken := []byte("package p\nfunc Broken( {\n")
	if err := os.WriteFile(src, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	orch.extractFn = func(lang, source, store string) (ExtractResult, error) {
		return ExtractResult{}, errors.New("injected parse failure")
	}
	defer func() { orch.extractFn = nil }()

	n, err := orch.indexFile(src, "go")
	if err != nil {
		t.Fatalf("indexFile should swallow extract failure: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 nodes on parse failure, got %d", n)
	}
	hs, _ = database.GetNodeByName("Hello")
	if len(hs) != 1 {
		t.Fatalf("old symbol was wiped on parse failure: %d", len(hs))
	}
	// Meta touched so full scans don't retry the broken file every pass.
	key := db.StoragePath(root, src)
	same, herr := database.FileHasContentHash(key, hashContent(broken))
	if herr != nil || !same {
		t.Fatalf("expected touched content_hash after parse failure: same=%v err=%v", same, herr)
	}
}

func TestIndexFileEmptyResultClearsOldIndex(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	src := filepath.Join(root, "a.go")
	if err := os.WriteFile(src, []byte("package p\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orch := NewOrchestrator(database, root)
	if _, err := orch.IndexFile(src); err != nil {
		t.Fatal(err)
	}
	hs, _ := database.GetNodeByName("Hello")
	if len(hs) != 1 {
		t.Fatalf("setup: expected Hello indexed, got %d", len(hs))
	}

	// Successful extract with zero symbols must clear the old ones (unlike a
	// failed parse). Content changes so the content-hash gate passes.
	empty := []byte("// nothing here\n")
	if err := os.WriteFile(src, empty, 0o644); err != nil {
		t.Fatal(err)
	}
	orch.extractFn = func(lang, source, store string) (ExtractResult, error) {
		return ExtractResult{}, nil // success, empty result
	}
	defer func() { orch.extractFn = nil }()

	n, err := orch.indexFile(src, "go")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 nodes, got %d", n)
	}
	hs, _ = database.GetNodeByName("Hello")
	if len(hs) != 0 {
		t.Fatalf("old symbol should be cleared on successful empty extract, got %d", len(hs))
	}
	// File record written with node_count=0 (successful empty extract).
	key := db.StoragePath(root, src)
	nodeCount, err := database.GetFileNodeCount(key)
	if err != nil || nodeCount != 0 {
		t.Fatalf("want node_count=0 file record, got %d err=%v", nodeCount, err)
	}
}

// ---- A5: same-size same-mtime content change must reindex ----

func TestSameSizeSameMtimeContentChangedReindexes(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	src := filepath.Join(root, "a.go")
	bodyA := []byte("package p\nfunc Hello() {}\n")
	bodyB := []byte("package p\nfunc World() {}\n") // same length as bodyA
	if len(bodyA) != len(bodyB) {
		t.Fatalf("test setup: bodies must have equal size")
	}
	if err := os.WriteFile(src, bodyA, 0o644); err != nil {
		t.Fatal(err)
	}

	orch := NewOrchestrator(database, root)
	if _, err := orch.IndexFile(src); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite with same size, restore the original mtime.
	if err := os.WriteFile(src, bodyB, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, time.Now(), info1.ModTime()); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if info2.Size() != info1.Size() {
		t.Fatal("test setup: sizes differ")
	}
	if info2.ModTime().UnixMilli() != info1.ModTime().UnixMilli() {
		t.Fatal("test setup: mtime (ms) differs")
	}

	// Metadata gate says unchanged → content-hash gate must detect the edit.
	files, nodes, err := orch.IndexAll()
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || nodes == 0 {
		t.Fatalf("same-size same-mtime edit must reindex: files=%d nodes=%d", files, nodes)
	}
	ws, _ := database.GetNodeByName("World")
	if len(ws) != 1 {
		t.Fatalf("expected World indexed after reindex, got %d", len(ws))
	}

	// IndexChanges path takes the same gate.
	bodyC := []byte("package p\nfunc Again() {}\n") // same length again
	if len(bodyC) != len(bodyB) {
		t.Fatal("test setup: bodyC size mismatch")
	}
	if err := os.WriteFile(src, bodyC, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, time.Now(), info1.ModTime()); err != nil {
		t.Fatal(err)
	}
	info3, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if info3.ModTime().UnixMilli() != info1.ModTime().UnixMilli() {
		t.Fatal("test setup: mtime (ms) differs (2)")
	}
	files, nodes, err = orch.IndexChanges([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || nodes == 0 {
		t.Fatalf("IndexChanges must reindex same-size same-mtime edit: files=%d nodes=%d", files, nodes)
	}
	as, _ := database.GetNodeByName("Again")
	if len(as) != 1 {
		t.Fatalf("expected Again indexed after IndexChanges, got %d", len(as))
	}
}

// ---- A6: IndexChanges drops the index for deleted/renamed-away files ----

func TestIndexChangesDeletesRemovedFile(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	src := filepath.Join(root, "a.go")
	if err := os.WriteFile(src, []byte("package p\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orch := NewOrchestrator(database, root)
	orch.SetForceReindex(true)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}
	hs, _ := database.GetNodeByName("Hello")
	if len(hs) != 1 {
		t.Fatalf("setup: expected Hello indexed, got %d", len(hs))
	}

	// Delete the file; IndexChanges must remove the stale index, not skip.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if _, _, err := orch.IndexChanges([]string{src}); err != nil {
		t.Fatal(err)
	}
	hs, _ = database.GetNodeByName("Hello")
	if len(hs) != 0 {
		t.Fatalf("stale index after delete: %d rows", len(hs))
	}
	files, err := database.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == db.StoragePath(root, src) {
			t.Fatal("file record should be removed after delete")
		}
	}
}

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
	orch.extractFn = func(lang, source, store string) (ExtractResult, bool, error) {
		return ExtractResult{}, false, errors.New("injected parse failure")
	}
	defer func() { orch.extractFn = nil }()

	n, err := orch.indexFile(src, "go", nil)
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
	orch.extractFn = func(lang, source, store string) (ExtractResult, bool, error) {
		return ExtractResult{}, false, nil // success, empty result
	}
	defer func() { orch.extractFn = nil }()

	n, err := orch.indexFile(src, "go", nil)
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

// TestIndexFileTSErrorRegexEmptyKeepsOldIndex: tree-sitter reports a real
// parse error AND the regex fallback finds nothing, while the file already
// has an index with symbols — the old symbols must be kept (S1). The failure
// must not ClearFile, must not write a node_count=0 record, must touch the
// file meta (so full scans don't retry every pass), and must return nil so
// IndexAll continues with other files.
func TestIndexFileTSErrorRegexEmptyKeepsOldIndex(t *testing.T) {
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

	// Content changes (passes the content-hash gate), then tree-sitter
	// errors while the regex fallback yields an empty result.
	broken := []byte("package p\nfunc Broken( {\n")
	if err := os.WriteFile(src, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	orch.extractFn = func(lang, source, store string) (ExtractResult, bool, error) {
		return ExtractResult{}, true, nil // ts error + regex empty
	}
	defer func() { orch.extractFn = nil }()

	n, err := orch.indexFile(src, "go", nil)
	if err != nil {
		t.Fatalf("indexFile should treat ts-error+empty as recoverable: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 nodes on failed extraction, got %d", n)
	}
	// Old symbols preserved — the production-path hollow-out bug cleared them.
	hs, _ = database.GetNodeByName("Hello")
	if len(hs) != 1 {
		t.Fatalf("old symbol was wiped on ts-error + empty regex fallback: %d", len(hs))
	}
	// No node_count=0 record: the stored count still reflects the old index.
	key := db.StoragePath(root, src)
	nodeCount, err := database.GetFileNodeCount(key)
	if err != nil || nodeCount == 0 {
		t.Fatalf("node_count must stay >0 on failed extraction, got %d err=%v", nodeCount, err)
	}
	// Meta touched so full scans don't retry the broken file every pass.
	same, herr := database.FileHasContentHash(key, hashContent(broken))
	if herr != nil || !same {
		t.Fatalf("expected touched content_hash after failed extraction: same=%v err=%v", same, herr)
	}
}

// TestIndexFileTSErrorRegexEmptyCountQueryFailsKeepsOldIndex: tree-sitter
// errors, the regex fallback is empty, and the stored node-count query itself
// fails (cerr != nil) — the orchestrator must NOT fall into the
// "successful empty result clears the old index" path. It keeps the old
// symbols, touches meta, and returns nil: never gamble on clearing an index
// it could not inspect.
func TestIndexFileTSErrorRegexEmptyCountQueryFailsKeepsOldIndex(t *testing.T) {
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

	// Content changes (passes the content-hash gate), then tree-sitter errors
	// with an empty regex fallback while the node-count query fails.
	broken := []byte("package p\nfunc Broken( {\n")
	if err := os.WriteFile(src, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	orch.extractFn = func(lang, source, store string) (ExtractResult, bool, error) {
		return ExtractResult{}, true, nil // ts error + regex empty
	}
	orch.nodeCountFn = func(path string) (int, error) {
		return 0, errors.New("injected count query failure")
	}
	defer func() {
		orch.extractFn = nil
		orch.nodeCountFn = nil
	}()

	n, err := orch.indexFile(src, "go", nil)
	if err != nil {
		t.Fatalf("indexFile should treat count-query failure as recoverable: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 nodes on failed extraction, got %d", n)
	}
	// Old symbols preserved — a failed count query must not clear them.
	hs, _ = database.GetNodeByName("Hello")
	if len(hs) != 1 {
		t.Fatalf("old symbol was wiped when node-count query failed: %d", len(hs))
	}
	// No node_count=0 record: the stored count still reflects the old index.
	key := db.StoragePath(root, src)
	nodeCount, err := database.GetFileNodeCount(key)
	if err != nil || nodeCount == 0 {
		t.Fatalf("node_count must stay >0 when count query failed, got %d err=%v", nodeCount, err)
	}
	// Meta touched so full scans don't retry the broken file every pass.
	same, herr := database.FileHasContentHash(key, hashContent(broken))
	if herr != nil || !same {
		t.Fatalf("expected touched content_hash after failed extraction: same=%v err=%v", same, herr)
	}
}

// TestIndexFileTSErrorRegexNonEmptyProceeds: tree-sitter errors but the regex
// fallback finds symbols — the fallback result must be indexed (the failure
// path only triggers when the fallback is also empty).
func TestIndexFileTSErrorRegexNonEmptyProceeds(t *testing.T) {
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
	if err := os.WriteFile(src, []byte("package p\nfunc Broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orch.extractFn = func(lang, source, store string) (ExtractResult, bool, error) {
		return ExtractResult{Nodes: []ExtractedNode{
			{Kind: "function", Name: "FallbackFn", File: store, Line: 3},
		}}, true, nil // ts error, but regex found a symbol
	}
	defer func() { orch.extractFn = nil }()

	n, err := orch.indexFile(src, "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 node from fallback, got %d", n)
	}
	fs, _ := database.GetNodeByName("FallbackFn")
	if len(fs) != 1 {
		t.Fatalf("fallback symbol should be indexed, got %d", len(fs))
	}
	// The old index was replaced by the fallback results (not kept).
	hs, _ := database.GetNodeByName("Hello")
	if len(hs) != 0 {
		t.Fatalf("old symbol should be replaced by fallback results, got %d", len(hs))
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

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// TestToolAffectedRejectsSymlinkEscape: an input path that passes the lexical
// jail but resolves through a symlink to a location OUTSIDE the workspace must
// be skipped — a test file outside the workspace must never be reported as
// affected, and fileToPackage must never read go.mod/package.json through the
// escaping link. Both an existing file reached via the link and a missing tail
// under the link (deleted file) are covered.
func TestToolAffectedRejectsSymlinkEscape(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// A test file OUTSIDE the workspace — must never appear in the result.
	if err := os.WriteFile(filepath.Join(outside, "secret_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "pkg", "a.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "pkg", "a_test.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertFile("pkg/a.go", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertFile("pkg/a_test.go", 0, 0); err != nil {
		t.Fatal(err)
	}

	evil := filepath.Join(ws, "evil")
	if err := os.Symlink(outside, evil); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	// Legit input + existing outside file via the link + missing tail under
	// the link (deleted file that must NOT be followed through the escape).
	res, err := ToolAffected(context.Background(), database, ws, AffectedArgs{
		Files: []string{"pkg/a.go", "evil/secret_test.go", "evil/gone_test.go"},
	})
	if err != nil {
		t.Fatalf("tool affected: %v", err)
	}
	text := ""
	if res != nil && len(res.Content) > 0 {
		text = res.Content[0].Text
	}
	head := text
	if i := strings.Index(text, "skipped"); i >= 0 {
		head = text[:i]
	}
	if strings.Contains(head, "evil") {
		t.Fatalf("symlink-escape paths leaked into the affected set:\n%s", text)
	}
	if !strings.Contains(text, "pkg/a_test.go") {
		t.Fatalf("legit same-package test missing from the result:\n%s", text)
	}
	if !strings.Contains(text, "skipped 2 input(s)") {
		t.Fatalf("expected skipped-input note, got:\n%s", text)
	}
}

func TestToolAffectedAllInputsDiscarded(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(ws, "evil")
	if err := os.Symlink(outside, evil); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	res, err := ToolAffected(context.Background(), database, ws, AffectedArgs{
		Files: []string{"../outside/x.go", "evil/secret_test.go"},
	})
	if err != nil {
		t.Fatalf("tool affected: %v", err)
	}
	text := ""
	if res != nil && len(res.Content) > 0 {
		text = res.Content[0].Text
	}
	if strings.Contains(text, "No affected test files found.") {
		t.Fatalf("all-discarded must not look like an empty test set:\n%s", text)
	}
	if !strings.Contains(text, "discarded") {
		t.Fatalf("expected discarded-inputs message, got:\n%s", text)
	}
	if !strings.Contains(text, "lexical") && !strings.Contains(text, "symlink") {
		t.Fatalf("expected skip reasons, got:\n%s", text)
	}
}

// TestToolAffectedKeepsDeletedFileInput: a deleted file (exists in the DB but
// not on disk — the git-assist use case) must still be accepted as an input
// and drive the traversal; rejecting it would silently drop affected tests.
func TestToolAffectedKeepsDeletedFileInput(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	workdir := t.TempDir()
	// go.mod makes fileToPackage resolve to module path + rel dir.
	if err := os.WriteFile(filepath.Join(workdir, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workdir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a.go does NOT exist on disk (deleted); the DB still holds its rows and
	// a_test.go imports the same package.
	if err := database.UpsertFile("pkg/a.go", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertFile("pkg/a_test.go", 0, 0); err != nil {
		t.Fatal(err)
	}
	mod, _ := database.UpsertNode(&db.Node{Kind: "module", Name: "example.com/x/pkg", File: "example.com/x/pkg", Line: 0})
	fileT, _ := database.UpsertNode(&db.Node{Kind: db.KindFile, Name: "pkg/a_test.go", File: "pkg/a_test.go", Line: 0})
	if _, err := database.UpsertEdge(&db.Edge{SourceID: fileT, TargetID: mod, Kind: db.EdgeImports, File: "pkg/a_test.go", Line: 1}); err != nil {
		t.Fatal(err)
	}

	res, err := ToolAffected(context.Background(), database, workdir, AffectedArgs{
		Files: []string{"pkg/a.go"}, // deleted on disk
	})
	if err != nil {
		t.Fatalf("tool affected: %v", err)
	}
	text := ""
	if res != nil && len(res.Content) > 0 {
		text = res.Content[0].Text
	}
	if !strings.Contains(text, "pkg/a_test.go") {
		t.Fatalf("deleted input must still drive the traversal; pkg/a_test.go missing:\n%s", text)
	}
}

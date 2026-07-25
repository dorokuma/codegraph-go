package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
)

// Real indexer stores workdir-relative paths; same-package tests must still appear.
func TestAffectedRealIndexSamePackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg/a.go"), []byte("package pkg\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg/a_test.go"), []byte("package pkg\n\nimport \"testing\"\nfunc TestHello(t *testing.T) { Hello() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	abs, _ := filepath.Abs(dir)
	database, err := db.Open(abs)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := extraction.NewOrchestrator(database, abs)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}
	files, _ := database.ListFiles()
	for _, p := range files {
		if filepath.IsAbs(p) {
			t.Fatalf("expected relative storage key, got absolute %q", p)
		}
	}

	res, err := ToolAffected(context.Background(), database, abs, AffectedArgs{Files: []string{"pkg/a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	if res != nil && len(res.Content) > 0 {
		text = res.Content[0].Text
	}
	if !strings.Contains(text, "a_test.go") {
		t.Fatalf("same-package test missing under real index:\n%s", text)
	}
}

func TestAffectedStdinCLIDoesNotHardReject(t *testing.T) {
	// CLI path may set Stdin; empty stdin + explicit files must still work.
	// MCP rejection lives in server.toolAffected, not here.
	database, cleanup := setupTestDB(t)
	defer cleanup()
	workdir := "/workdir"
	_ = database.UpsertFile("pkg/a.go", 0, 0)
	_ = database.UpsertFile("pkg/a_test.go", 0, 0)
	res, err := ToolAffected(context.Background(), database, workdir, AffectedArgs{
		Stdin: true,
		Files: []string{"pkg/a.go"},
	})
	if err != nil {
		t.Fatalf("CLI stdin+files should not hard-reject: %v", err)
	}
	if res == nil {
		t.Fatal("expected result")
	}
}

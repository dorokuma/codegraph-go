package tools

import (
	"context"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
)

func TestToolAffectedNoFiles(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	_, err := ToolAffected(context.Background(), database, "/workdir", AffectedArgs{Files: []string{}})
	if err == nil {
		t.Fatal("expected error for empty files")
	}
}

func TestToolAffectedWithTestFiles(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert some import relationships
	fileA, _ := database.UpsertNode(&db.Node{Kind: db.KindFile, Name: "/workdir/pkg/a.go", File: "/workdir/pkg/a.go", Line: 0})
	fileTest, _ := database.UpsertNode(&db.Node{Kind: db.KindFile, Name: "/workdir/pkg/a_test.go", File: "/workdir/pkg/a_test.go", Line: 0})
	mod, _ := database.UpsertNode(&db.Node{Kind: "module", Name: "github.com/test/pkg", File: "github.com/test/pkg", Line: 0})

	database.UpsertEdge(&db.Edge{SourceID: fileA, TargetID: mod, Kind: db.EdgeImports, File: "/workdir/pkg/a.go", Line: 1})
	database.UpsertEdge(&db.Edge{SourceID: fileTest, TargetID: mod, Kind: db.EdgeImports, File: "/workdir/pkg/a_test.go", Line: 1})

	result, err := ToolAffected(context.Background(), database, "/workdir", AffectedArgs{
		Files: []string{"pkg/a.go"},
	})
	if err != nil {
		t.Fatalf("tool affected: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected result")
	}

	// Should find test files
	text := result.Content[0].Text
	t.Logf("Result: %s", text)
}

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		file    string
		workdir string
		want    bool
	}{
		{"/workdir/pkg/a_test.go", "/workdir", true},
		{"/workdir/pkg/test_a.py", "/workdir", true},
		{"/workdir/pkg/a_test.py", "/workdir", true},
		{"/workdir/pkg/a.test.ts", "/workdir", true},
		{"/workdir/pkg/a.spec.ts", "/workdir", true},
		{"/workdir/pkg/a_test.go", "/workdir", true},
		{"/workdir/tests/a.go", "/workdir", true},
		{"/workdir/test/a.go", "/workdir", true},
		{"/workdir/__tests__/a.js", "/workdir", true},
		{"/workdir/pkg/a.go", "/workdir", false},
		{"/workdir/pkg/main.go", "/workdir", false},
		{"/workdir/pkg/app.ts", "/workdir", false},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := isTestFile(tt.file, tt.workdir, "")
			if got != tt.want {
				t.Errorf("isTestFile(%q) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

func TestIsTestFileWithCustomFilter(t *testing.T) {
	tests := []struct {
		file    string
		filter  string
		want    bool
	}{
		{"/workdir/pkg/foo_test.go", "*_test.go", true},
		{"/workdir/pkg/bar_test.go", "*_test.go", true},
		{"/workdir/pkg/baz.go", "*_test.go", false},
		{"/workdir/pkg/spec_foo.ts", "spec_*", true},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := isTestFile(tt.file, "/workdir", tt.filter)
			if got != tt.want {
				t.Errorf("isTestFile(%q, %q) = %v, want %v", tt.file, tt.filter, got, tt.want)
			}
		})
	}
}

func TestUnique(t *testing.T) {
	tests := []struct {
		input []string
		want  int
	}{
		{[]string{"a", "b", "c"}, 3},
		{[]string{"a", "a", "b"}, 2},
		{[]string{"a", "a", "a"}, 1},
		{[]string{}, 0},
	}

	for _, tt := range tests {
		got := unique(tt.input)
		if len(got) != tt.want {
			t.Errorf("unique(%v) = %d, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestTestPatterns(t *testing.T) {
	// Verify test patterns are defined for common languages
	expectedLangs := []string{"go", "typescript", "javascript", "python", "rust", "java", "csharp"}
	for _, lang := range expectedLangs {
		if _, ok := testPatterns[lang]; !ok {
			t.Errorf("missing test patterns for %s", lang)
		}
	}
}

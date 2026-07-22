package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestResolvePath(t *testing.T) {
	s := &server{workdir: "/workdir"}

	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "/workdir", false},
		{"subdir", "/workdir/subdir", false},
		{"subdir/nested", "/workdir/subdir/nested", false},
		{"../outside", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := s.resolvePath(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolvePath(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("resolvePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripStringsAndComments(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "hello world"},
		{"// comment", "          "},
		{"'c'", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripStringsAndComments(tt.input)
			if got != tt.want {
				t.Errorf("stripStringsAndComments(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCountLeadingSpaces(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"hello", 0},
		{"  hello", 2},
		{"\thello", 1},
		{"    hello", 4},
		{"", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := countLeadingSpaces(tt.input)
			if got != tt.want {
				t.Errorf("countLeadingSpaces(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateOutputUTF8(t *testing.T) {
	// Test with multi-byte UTF-8 characters
	input := strings.Repeat("你", 10000) // Each char is 3 bytes
	result := truncateOutput(input, 100)
	// The result should be truncated and valid UTF-8
	if !strings.Contains(result, "truncated") {
		t.Error("should mention truncated")
	}
	// Verify the result is valid UTF-8
	if !isValidUTF8(result) {
		t.Error("result should be valid UTF-8")
	}

	// Verify that invalid UTF-8 is detected as false
	invalid := string([]byte{0xff, 0xfe, 0xfd})
	if isValidUTF8(invalid) {
		t.Error("invalid UTF-8 should be detected as false")
	}
}

func isValidUTF8(s string) bool {
	return utf8.ValidString(s)
}

func TestLimitLinesEdgeCases(t *testing.T) {
	// Empty string
	result := limitLines("", 5)
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}

	// n <= 0
	result = limitLines("a\nb", 0)
	if result != "a\nb" {
		t.Errorf("expected passthrough, got %q", result)
	}

	// Fewer lines than limit
	result = limitLines("a\nb", 5)
	if result != "a\nb" {
		t.Errorf("expected passthrough, got %q", result)
	}
}

func TestSearchPerFileCapEdgeCases(t *testing.T) {
	// Zero global
	result := searchPerFileCap(0)
	if result != defaultSearchPerFile {
		t.Errorf("expected default %d, got %d", defaultSearchPerFile, result)
	}

	// Negative global
	result = searchPerFileCap(-1)
	if result != defaultSearchPerFile {
		t.Errorf("expected default %d, got %d", defaultSearchPerFile, result)
	}
}

func TestAddStalenessWarning(t *testing.T) {
	s := &server{workdir: "/workdir"}
	// no watcher
	if got := s.addStalenessWarning("ok"); got != "ok" {
		t.Fatalf("expected unchanged without watcher, got %q", got)
	}
}

func TestResolveProjectDefaultAndNearest(t *testing.T) {
	base := t.TempDir()
	// default session index
	def := filepath.Join(base, "default")
	os.MkdirAll(def, 0o755)
	defDB, err := db.Open(def)
	if err != nil {
		t.Fatal(err)
	}
	defer defDB.Close()

	// separate project with its own index
	other := filepath.Join(base, "other")
	os.MkdirAll(filepath.Join(other, "pkg"), 0o755)
	otherDB, err := db.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	otherDB.Close()

	s := &server{workdir: def, database: defDB}

	root, database, err := s.resolveProject("")
	if err != nil || root != def || database != defDB {
		t.Fatalf("default: root=%q err=%v", root, err)
	}

	root, database, err = s.resolveProject(filepath.Join(other, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if root != other {
		t.Fatalf("nearest other = %q want %q", root, other)
	}
	if database == defDB {
		t.Fatal("should open a different DB for other project")
	}
	s.closeProjectCache()

	// unindexed path
	lonely := filepath.Join(base, "lonely")
	os.MkdirAll(lonely, 0o755)
	if _, _, err := s.resolveProject(lonely); err == nil {
		t.Fatal("expected error for unindexed projectPath")
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "src/main.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "main.ts", false},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/pkg/util.go", true},
		{"src/**/*.go", "other/main.go", false},
		{"**/*.test.ts", "src/foo.test.ts", true},
		{"**/*.test.ts", "foo.test.ts", true},
		{"**/*.test.ts", "foo.ts", false},
		{"*.go", "", false},
	}
	for _, tt := range tests {
		got := globMatch(tt.pattern, tt.path)
		if got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

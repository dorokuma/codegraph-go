package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestResolvePath(t *testing.T) {
	// resolvePathIn now rejects nonexistent targets (no ghost paths handed to
	// rg), so the workspace and the resolved paths must exist on disk.
	base := t.TempDir()
	ws := filepath.Join(base, "workdir")
	if err := os.MkdirAll(filepath.Join(ws, "subdir", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{Workdir: ws, Workdirs: []string{ws}}

	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", ws, false},
		{"subdir", filepath.Join(ws, "subdir"), false},
		{"subdir/nested", filepath.Join(ws, "subdir", "nested"), false},
		{"subdir/missing", "", true}, // nonexistent tail → rejected, not a ghost path
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

// TestResolvePathInPerRootGhostPath: per-root daemon (workdir IS the project
// dir, e.g. /root/codegraph-go) receiving search path=codegraph-go joins into
// /root/codegraph-go/codegraph-go, which does not exist. resolvePathIn must
// reject it with a clear "not found" error instead of returning the ghost
// path — rg would otherwise exit with status 2 ("No such file or
// directory"). Existing subdirectories must still resolve.
func TestResolvePathInPerRootGhostPath(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "codegraph-go")
	if err := os.MkdirAll(filepath.Join(ws, "internal", "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{Workdir: ws, Workdirs: []string{ws}}

	// Ghost path: workdir basename double-joined under itself → nonexistent.
	_, err := s.resolvePathIn(ws, "codegraph-go")
	if err == nil {
		t.Fatal("expected nonexistent path to be rejected, got no error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected clear not-found error, got: %v", err)
	}

	// Nonexistent tail under a real prefix is also rejected.
	if _, err := s.resolvePathIn(ws, "internal/nope"); err == nil {
		t.Fatal("expected nonexistent tail to be rejected")
	}

	// Existing subdirectory still resolves (normal-path regression guard).
	got, err := s.resolvePathIn(ws, "internal/server")
	if err != nil {
		t.Fatalf("existing subdir rejected: %v", err)
	}
	if want := filepath.Join(ws, "internal", "server"); got != want {
		t.Fatalf("resolvePathIn = %q, want %q", got, want)
	}

	// The workdir itself resolves via empty path and ".".
	if got, err := s.resolvePathIn(ws, ""); err != nil || got != ws {
		t.Fatalf("resolvePathIn('') = %q, %v; want %q", got, err, ws)
	}
	if got, err := s.resolvePathIn(ws, "."); err != nil || got != ws {
		t.Fatalf("resolvePathIn('.') = %q, %v; want %q", got, err, ws)
	}
}

// TestResolvePathInMainLibProjectPrefix: home/main-lib mode (workdir = parent,
// e.g. /root) with an existing project subdirectory must keep resolving
// project-level queries like search path=codegraph-go.
func TestResolvePathInMainLibProjectPrefix(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workdir")
	proj := filepath.Join(ws, "codegraph-go")
	if err := os.MkdirAll(filepath.Join(proj, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{Workdir: ws, Workdirs: []string{ws}}

	got, err := s.resolvePathIn(ws, "codegraph-go")
	if err != nil {
		t.Fatalf("existing project dir rejected: %v", err)
	}
	if got != proj {
		t.Fatalf("resolvePathIn = %q, want %q", got, proj)
	}
}

func TestResolvePathSymlinkEscape(t *testing.T) {
	// B6/W8: a symlink inside the workspace pointing outside must be rejected
	// by resolvePath/resolvePathIn (search/files/explore all go through it).
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(ws, "evil")
	if err := os.Symlink(outside, evil); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(ws, "good")
	inner := filepath.Join(ws, "sub")
	if err := os.Symlink(inner, good); err != nil {
		t.Fatal(err)
	}

	s := &Server{Workdir: ws, Workdirs: []string{ws}}

	// Direct link target outside → rejected.
	if _, err := s.resolvePathIn(ws, "evil"); err == nil {
		t.Fatal("expected symlink escape (evil) to be rejected")
	}
	// Child path through the escaping link → rejected.
	if _, err := s.resolvePathIn(ws, "evil/file.txt"); err == nil {
		t.Fatal("expected symlink escape (evil/file.txt) to be rejected")
	}
	// Nonexistent tail under the escaping link → rejected (ancestor resolved).
	if _, err := s.resolvePathIn(ws, "evil/notyet"); err == nil {
		t.Fatal("expected symlink escape (evil/notyet) to be rejected")
	}
	// resolvePath across workdirs → rejected too.
	if _, err := s.resolvePath("evil"); err == nil {
		t.Fatal("expected resolvePath to reject symlink escape")
	}
	// Absolute path through the escaping link → rejected.
	if _, err := s.resolvePathIn(ws, evil); err == nil {
		t.Fatal("expected absolute symlink escape to be rejected")
	}

	// Symlink to a location INSIDE the workspace → allowed, returns the real path.
	got, err := s.resolvePathIn(ws, "good")
	if err != nil {
		t.Fatalf("internal symlink rejected: %v", err)
	}
	if got != inner {
		t.Fatalf("resolvePathIn(good) = %q, want %q", got, inner)
	}

	// Ordinary paths inside the workspace still resolve.
	got, err = s.resolvePathIn(ws, "sub")
	if err != nil || got != inner {
		t.Fatalf("resolvePathIn(sub) = %q err=%v, want %q", got, err, inner)
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
	s := &Server{Workdir: "/workdir", Workdirs: []string{"/workdir"}}
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

	// separate project with its own index, INSIDE the configured workdir
	other := filepath.Join(def, "other")
	os.MkdirAll(filepath.Join(other, "pkg"), 0o755)
	otherDB, err := db.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	otherDB.Close()

	s := &Server{Workdir: def, Workdirs: []string{def}, Database: defDB}

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

	// unindexed path (no .codegraph anywhere up the tree → error before the
	// workdir whitelist even applies)
	lonely := filepath.Join(base, "lonely")
	os.MkdirAll(lonely, 0o755)
	if _, _, err := s.resolveProject(lonely); err == nil {
		t.Fatal("expected error for unindexed projectPath")
	}
}

func TestResolveProjectWorkdirWhitelist(t *testing.T) {
	// B7/W9: projectPath must resolve to a project root inside the configured
	// workdirs; indexed projects elsewhere are rejected.
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "proj", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsDB, err := db.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer wsDB.Close()
	// indexed project inside the workdir
	projDB, err := db.Open(filepath.Join(ws, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	projDB.Close()
	// indexed project OUTSIDE the workdirs
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	outDB, err := db.Open(outside)
	if err != nil {
		t.Fatal(err)
	}
	outDB.Close()

	s := &Server{Workdir: ws, Workdirs: []string{ws}, Database: wsDB}

	// Inside → accepted.
	root, database, err := s.resolveProject(filepath.Join(ws, "proj", "pkg"))
	if err != nil {
		t.Fatalf("inside project rejected: %v", err)
	}
	if root != filepath.Join(ws, "proj") {
		t.Fatalf("root = %q", root)
	}
	s.releaseProject(root)
	s.closeProjectCache()

	// Outside → rejected with a clear error.
	if _, _, err := s.resolveProject(outside); err == nil {
		t.Fatal("expected outside project to be rejected")
	} else if !strings.Contains(err.Error(), "outside configured workdirs") {
		t.Fatalf("error %q should mention configured workdirs", err)
	}

	// Default (empty projectPath) still works.
	root, database, err = s.resolveProject("")
	if err != nil || root != ws || database != wsDB {
		t.Fatalf("default: root=%q err=%v", root, err)
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

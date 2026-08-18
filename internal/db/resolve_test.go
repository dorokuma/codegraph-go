package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBestTargetSameFile(t *testing.T) {
	cands := []Node{
		{ID: 1, Name: "Close", File: "/a/other.go", Kind: KindFunction},
		{ID: 2, Name: "Close", File: "/a/db.go", Kind: KindMethod, Body: "func (r *Rows) Close() {}"},
	}
	got := ResolveBestTarget(cands, "/a/db.go", true)
	if got != 2 {
		t.Fatalf("want same-file id=2, got %d", got)
	}
}

func TestResolveBestTargetSkipsFileKind(t *testing.T) {
	cands := []Node{
		{ID: 1, Name: "fmt", File: "fmt", Kind: "module"},
		{ID: 2, Name: "fmt", File: "/x/fmt.go", Kind: KindFunction, Body: "func fmt() {}"},
	}
	got := ResolveBestTarget(cands, "/x/main.go", true)
	if got != 2 {
		t.Fatalf("want function id=2, got %d", got)
	}
}

func TestResolveBestTargetAmbiguousCeiling(t *testing.T) {
	var cands []Node
	for i := int64(1); i <= 100; i++ {
		cands = append(cands, Node{ID: i, Name: "init", File: "/x/f.go", Kind: KindFunction})
	}
	// none same-file as fromFile
	got := ResolveBestTarget(cands, "/other/z.go", true)
	if got != 0 {
		t.Fatalf("want 0 for ubiquitous name, got %d", got)
	}
}

func TestTruncateBody(t *testing.T) {
	if TruncateBody("short") != "short" {
		t.Fatal("short unchanged")
	}
	long := make([]byte, MaxBodyChars+100)
	for i := range long {
		long[i] = 'a'
	}
	out := TruncateBody(string(long))
	if len(out) > MaxBodyChars+40 {
		t.Fatalf("not truncated enough: %d", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("missing marker")
	}
}

func TestStoragePathAndAbsPathRoundTrip(t *testing.T) {
	wd := "/proj/root"
	got := StoragePath(wd, filepath.Join(wd, "pkg", "a.go"))
	if got != "pkg/a.go" {
		t.Fatalf("StoragePath abs → %q want pkg/a.go", got)
	}
	got = StoragePath(wd, "pkg/a.go")
	if got != "pkg/a.go" {
		t.Fatalf("StoragePath rel → %q want pkg/a.go", got)
	}
	abs := AbsPath(wd, "pkg/a.go")
	if abs != filepath.Join(wd, "pkg", "a.go") {
		t.Fatalf("AbsPath → %q", abs)
	}
	if RelPath(wd, "pkg/a.go") != "pkg/a.go" {
		t.Fatalf("RelPath relative key")
	}
	if RelPath(wd, filepath.Join(wd, "pkg", "a.go")) != "pkg/a.go" {
		t.Fatalf("RelPath absolute")
	}
}

func TestPathUnderRootRelativeKeyVsAbsFilter(t *testing.T) {
	wd := "/proj/root"
	// Production index keys are workdir-relative.
	if !PathUnderRoot("internal/server/tools.go", wd, filepath.Join(wd, "internal", "server")) {
		t.Fatal("abs path= filter must match relative stored key")
	}
	if !PathUnderRoot("internal/server/tools.go", wd, "internal/server") {
		t.Fatal("relative path= filter must match relative stored key")
	}
	if PathUnderRoot("internal/db/query.go", wd, "internal/server") {
		t.Fatal("sibling dir must not match")
	}
	if !PathUnderRoot("internal/server/tools.go", wd, wd) {
		t.Fatal("root==workdir is the whole tree")
	}
}

// TestAbsPathJailsEscapes: relative storage keys that would escape workdir
// (hand-crafted or malicious index rows) must be rejected with "" so callers
// doing disk reads through AbsPath cannot be tricked out of the workspace.
func TestAbsPathJailsEscapes(t *testing.T) {
	wd := "/proj/root"
	escapes := []string{
		"../../etc/passwd",
		"pkg/../../../x",
		"a/../../b",
		"..",
	}
	for _, s := range escapes {
		if got := AbsPath(wd, s); got != "" {
			t.Errorf("AbsPath(%q) = %q, want \"\" (escape must be rejected)", s, got)
		}
	}

	inside := map[string]string{
		"pkg/a.go":       filepath.Join(wd, "pkg", "a.go"),
		"pkg/../a.go":    filepath.Join(wd, "a.go"), // resolves inside → allowed
		"a":              filepath.Join(wd, "a"),
		".":              wd,
		wd + "/pkg/a.go": filepath.Join(wd, "pkg", "a.go"), // absolute inside
	}
	for s, want := range inside {
		if got := AbsPath(wd, s); got != want {
			t.Errorf("AbsPath(%q) = %q, want %q", s, got, want)
		}
	}
	// Absolute stored keys (legacy out-of-workdir files) are kept as-is.
	if got := AbsPath(wd, "/elsewhere/x.go"); got != "/elsewhere/x.go" {
		t.Errorf("absolute legacy key must pass through, got %q", got)
	}
}

func TestSqliteFileDSNEscapesSpecials(t *testing.T) {
	dsn := sqliteFileDSN("/tmp/weird path/db#1?.db")
	if !strings.Contains(dsn, "%20") || !strings.Contains(dsn, "%23") || !strings.Contains(dsn, "%3F") {
		t.Fatalf("expected escapes in DSN: %s", dsn)
	}
	if strings.Contains(dsn, "path/db#") {
		t.Fatalf("raw # should not remain: %s", dsn)
	}
}

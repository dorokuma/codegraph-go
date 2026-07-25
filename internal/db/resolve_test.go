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

func TestSqliteFileDSNEscapesSpecials(t *testing.T) {
	dsn := sqliteFileDSN("/tmp/weird path/db#1?.db")
	if !strings.Contains(dsn, "%20") || !strings.Contains(dsn, "%23") || !strings.Contains(dsn, "%3F") {
		t.Fatalf("expected escapes in DSN: %s", dsn)
	}
	if strings.Contains(dsn, "path/db#") {
		t.Fatalf("raw # should not remain: %s", dsn)
	}
}

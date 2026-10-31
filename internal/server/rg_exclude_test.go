package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/config"
	"github.com/dorokuma/codegraph-go/internal/db"
)

// TestRgExcludeArgs: the helper maps each exclusion rule class to the
// matching rg negative glob pair (directory-only "<glob>/" + contents
// "<glob>/**") and honours the no_ignore opt-out. The built-in rules come
// from extraction.BuiltinSkipGlobs (single source of truth); the test
// asserts the four config classes land after them in order.
func TestRgExcludeArgs(t *testing.T) {
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"ref", "*-backup*", "./archive", "experiments/scratch", " "}, 0)

	got := rgExcludeArgs("/any/root", false)

	// The built-in single-source globs come first — node_modules, .wt,
	// code_references (bare names), **/.local/lib, **/go/pkg (fragments),
	// etc. They are sorted lexically by BuiltinSkipGlobs. Assert a few
	// representative built-ins are present as pairs, then the config tail.
	mustContainPair(t, got, "!node_modules/", "!node_modules/**")
	mustContainPair(t, got, "!.wt/", "!.wt/**")
	mustContainPair(t, got, "!code_references/", "!code_references/**")
	mustContainPair(t, got, "!**/.local/lib/", "!**/.local/lib/**")
	mustContainPair(t, got, "!**/go/pkg/", "!**/go/pkg/**")

	// Config rules land last, in declaration order, after the built-ins.
	tail := []string{
		"-g", "!ref/", "-g", "!ref/**",
		"-g", "!*-backup*/", "-g", "!*-backup*/**",
		"-g", "!/archive/", "-g", "!/archive/**",
		"-g", "!/experiments/scratch/", "-g", "!/experiments/scratch/**",
	}
	if len(got) < len(tail) || !reflect.DeepEqual(got[len(got)-len(tail):], tail) {
		t.Fatalf("rgExcludeArgs config tail = %v, want %v", got[len(got)-min(len(tail), len(got)):], tail)
	}

	if got := rgExcludeArgs("/any/root", true); got != nil {
		t.Fatalf("no_ignore must disable exclusion globs, got %v", got)
	}

	// noIgnore=true must not emit anything even with config rules.
	if got := rgExcludeArgs("/any/root", true); got != nil {
		t.Fatalf("no_ignore must disable all globs, got %v", got)
	}
}

// mustContainPair asserts a "!<g>/" + "!<g>/**" negative-glob pair appears
// consecutively somewhere in the -g flag list.
func mustContainPair(t *testing.T, args []string, dir, subtree string) {
	t.Helper()
	for i := 0; i+3 < len(args); i += 4 {
		if args[i] == "-g" && args[i+1] == dir && args[i+2] == "-g" && args[i+3] == subtree {
			return
		}
	}
	t.Fatalf("glob pair (%q, %q) not found in %v", dir, subtree, args)
}

// TestRgExcludeArgsFiltersMatches: end-to-end against a real rg — files
// under .wt and an exclude_dirs rule must not be matched while a normal
// file still is.
func TestRgExcludeArgsFiltersMatches(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main.go")
	write(".wt/repo/feat-x/src/dup.go")
	write("ref/data.go")
	write("proj/internal/ref/ok.go") // deep same-name under anchored rule: kept
	// Built-in skip trees the indexer would refuse: node_modules, .cache,
	// .local/lib (fragment), code_references (single-seg fragment), go/pkg.
	write("node_modules/pkg/lib/sub.go")
	write(".cache/a.go")
	write(".local/lib/b.go")
	write("code_references/c.go")
	write("go/pkg/mod/d.go")
	write("sub/go/pkg/e.go")
	// Same-named paths that must NOT be killed: a project dir *-go/pkg is
	// not a GOPATH segment, and sdk/go is not a toolchain dir.
	write("codegraph-go/pkg/keep.go")
	write("sdk/go/keep.go")

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"./ref"}, 0)

	args := append([]string{"--line-number", "--no-heading", "--color=never", "--fixed-strings"},
		rgExcludeArgs(root, false)...)
	args = append(args, "--", "needle", root)
	cmd := exec.Command("rg", args...)
	// "/"-anchored globs resolve against the child cwd, same contract the
	// production call sites honour (cmd.Dir = project root).
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			t.Fatalf("rg: %v", err)
		}
	}
	got := string(out)

	wantHit := []string{
		"src/main.go",
		"proj/internal/ref/ok.go",  // deep same-name under anchored rule: kept
		"codegraph-go/pkg/keep.go", // *-go/pkg is NOT a GOPATH segment
		"sdk/go/keep.go",           // sdk/go is NOT a toolchain dir
	}
	for _, w := range wantHit {
		if !strings.Contains(got, w) {
			t.Fatalf("expected %q in rg output, got: %q", w, got)
		}
	}
	wantMiss := []string{
		".wt/",          // .wt container
		"ref/data.go",   // anchored ./ref top-level dir
		"node_modules/", // built-in skip dir
		".cache/",       // built-in skip dir
		".local/lib/",   // skipPathFragments multi-seg
		"code_references/",
		"go/pkg/mod/d.go", // Go toolchain segment (top level)
		"sub/go/pkg/e.go", // Go toolchain segment (nested)
	}
	for _, m := range wantMiss {
		if strings.Contains(got, m) {
			t.Fatalf("%q must be excluded, got: %q", m, got)
		}
	}
}

// TestToolSearchNarrowedRootInSkipAncestor: the project root itself has
// been narrowed INTO a tree whose ancestor segments match built-in skip
// rules (/tmp/.../node_modules/pkg). The indexer protects the root via
// ShouldSkipDirIn's workdir-relative check — the rg path must do the same:
// files directly inside the narrowed root are still found, while a nested
// node_modules INSIDE it is still excluded. Regression for ancestor-path
// false positives when rg is handed an absolute path inside a skip tree.
func TestToolSearchNarrowedRootInSkipAncestor(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	base := t.TempDir()
	root := filepath.Join(base, "node_modules", "pkg")
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("lib")
	write("lib/sub.go", "package lib\nfunc Needle() {}\n")
	// A nested node_modules inside the narrowed root is still excluded.
	mkdir("lib/node_modules/dep")
	write("lib/node_modules/dep/x.go", "package dep\nfunc Needle() {}\n")

	s, _ := setupToolServerAt(t, root)

	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Needle"})
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "sub.go") {
		t.Fatalf("narrowed root must still surface lib/sub.go, got: %q", text)
	}
	if strings.Contains(text, "node_modules/dep/x.go") {
		t.Fatalf("nested node_modules must still be excluded, got: %q", text)
	}
}

// TestToolFilesNarrowedRootInSkipAncestor is the files-listing twin of
// TestToolSearchNarrowedRootInSkipAncestor: toolFiles (rg --files) with
// the project root narrowed into node_modules/pkg must list lib/sub.go
// but not the nested node_modules content.
func TestToolFilesNarrowedRootInSkipAncestor(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	base := t.TempDir()
	root := filepath.Join(base, "node_modules", "pkg")
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("lib")
	write("lib/sub.go", "package lib\n")
	mkdir("lib/node_modules/dep")
	write("lib/node_modules/dep/x.go", "package dep\n")

	s, _ := setupToolServerAt(t, root)

	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Pattern: "**", Max: 100})
	if err != nil {
		t.Fatalf("toolFiles: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "sub.go") {
		t.Fatalf("narrowed root must list lib/sub.go, got: %q", text)
	}
	if strings.Contains(text, "node_modules/dep/x.go") {
		t.Fatalf("nested node_modules must be excluded, got: %q", text)
	}
}

// TestToolSearchNarrowedRootInLocalLibFragment: same narrowed-root
// protection but the ancestor match is a multi-segment skipPathFragments
// entry (.local/lib) — the fragment glob "**/.local/lib" must not blank
// the narrowed root's own tree even though its absolute prefix contains
// /.local/lib/.
func TestToolSearchNarrowedRootInLocalLibFragment(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	base := t.TempDir()
	root := filepath.Join(base, ".local", "lib", "pkg")
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("lib")
	write("lib/sub.go", "package lib\nfunc Needle() {}\n")
	mkdir("lib/.cache/x")
	write("lib/.cache/x/c.go", "package c\nfunc Needle() {}\n")

	s, _ := setupToolServerAt(t, root)

	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Needle"})
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "sub.go") {
		t.Fatalf("narrowed .local/lib root must still surface lib/sub.go, got: %q", text)
	}
	if strings.Contains(text, ".cache/x/c.go") {
		t.Fatalf("nested .cache must be excluded, got: %q", text)
	}
}

// setupToolServerAt is setupToolServer with Workdir pointed at an arbitrary
// root (used by the narrowed-root tests where the root sits inside a skip
// ancestor tree). The DB lives in the same dir; Workdirs=[root].
func setupToolServerAt(t *testing.T, root string) (*Server, string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := &Server{
		Workdir:  root,
		Workdirs: []string{root},
		Database: database,
	}
	return s, root
}

// TestToolFilesAnchoredExcludeKeepsProjRoot: a narrowed files search
// (path=subproj) must still evaluate "/"-anchored exclusion globs against
// the PROJECT root, not the subdirectory. exclude_dirs=["./ref"] excludes
// <root>/ref but must leave <root>/subproj/ref searchable — and the output
// paths stay relative to the requested subdirectory.
func TestToolFilesAnchoredExcludeKeepsProjRoot(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	s, root := setupToolServer(t)
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("ref")
	mkdir("subproj/ref")
	write("ref/x.go", "package ref\n")
	write("subproj/ref/keep.go", "package ref\n")

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"./ref"}, 0)

	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Path: "subproj", Pattern: "**/*.go", Max: 50})
	if err != nil {
		t.Fatalf("toolFiles: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "ref/keep.go") {
		t.Fatalf("subproj/ref must NOT be excluded by anchored ./ref rule (rg dir must stay projRoot), got: %q", text)
	}
	if strings.Contains(text, "ref/x.go") {
		t.Fatalf("top-level ref/x.go must be excluded by anchored ./ref rule, got: %q", text)
	}
}

// TestToolFilesBareRuleDirOnly: a bare-name rule (exclude_dirs=["vendor2"])
// excludes the directory <root>/sub/vendor2/ but must not kill a plain
// FILE named vendor2 elsewhere in the tree — the "!<glob>/" gitignore
// directory semantics only match directories. (A file and a directory
// cannot share the same path, so the same-named file lives in a sibling
// dir; the gitignore pattern is name-based either way.)
func TestToolFilesBareRuleDirOnly(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	s, root := setupToolServer(t)
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("sub/vendor2")
	write("sub/vendor2/a.go", "package vendor2\n")
	mkdir("sub2")
	write("sub2/vendor2", "plain file, not a directory\n") // same-named regular file

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"vendor2"}, 0)

	// Search the whole project: the directory's contents are excluded…
	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Pattern: "**", Max: 100})
	if err != nil {
		t.Fatalf("toolFiles: %v", err)
	}
	text := textContent(res)
	if strings.Contains(text, "vendor2/a.go") {
		t.Fatalf("dir sub/vendor2 must be excluded by bare rule, got: %q", text)
	}
	// …while the same-named regular file survives (output line is the
	// root-relative path "sub2/vendor2").
	if !strings.Contains(text, "sub2/vendor2") {
		t.Fatalf("plain file named vendor2 must NOT be excluded by a directory rule, got: %q", text)
	}
}

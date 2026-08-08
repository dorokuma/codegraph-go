package extraction

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// ---- audit #1: Go multi-line import must produce imports edges on the
// tree-sitter main path (import_spec_list is nested under import_declaration),
// and the single-line form must keep working. ----

func TestTSGoMultiLineImport(t *testing.T) {
	source := `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Println(strings.Join(os.Args, " "))
}
`
	res, err := NewTreeSitterExtractor("go").Extract(source, "/main.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == "imports" {
			got[e.TargetName] = true
		}
	}
	for _, want := range []string{"fmt", "os", "strings"} {
		if !got[want] {
			t.Fatalf("multi-line import missing %q (edges=%v)", want, got)
		}
	}

	// Single-line import must still work.
	single := "package p\nimport \"fmt\"\nfunc f() { fmt.Println() }\n"
	res2, err := NewTreeSitterExtractor("go").Extract(single, "/single.go")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res2.Edges {
		if e.Kind == "imports" && e.TargetName == "fmt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("single-line import missing: %+v", res2.Edges)
	}
}

// ---- audit #4: CommonJS require(...) and dynamic import(...) must produce
// imports edges on the tree-sitter JS/TS path. ----

func TestTSJSRequireAndDynamicImport(t *testing.T) {
	source := `const a = require("./a");
const b = import("./b");
import c from "./c";
`
	res, err := NewTreeSitterExtractor("javascript").Extract(source, "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == "imports" {
			got[e.TargetName] = true
		}
	}
	for _, want := range []string{"./a", "./b", "./c"} {
		if !got[want] {
			t.Fatalf("import missing %q (edges=%v)", want, got)
		}
	}
}

// ---- audit #2: same-named methods must not steal each other's calls. Each
// call ref is stamped with the enclosing definition line (FromLine), picked
// by source-range containment, not by first-wins name lookup. ----

func TestTSSameNameMethodCallFromLine(t *testing.T) {
	// Line 1: package main
	// Line 3: func (a A) String() string { return "A" }
	// Line 4: func (b B) String() string { return "B" }
	// Line 5: func (a A) Use() string { return a.String() }
	// Line 6: func (b B) Use() string { return b.String() }
	source := `package main

func (a A) String() string { return "A" }
func (b B) String() string { return "B" }
func (a A) Use() string { return a.String() }
func (b B) Use() string { return b.String() }
`
	res, err := NewTreeSitterExtractor("go").Extract(source, "/a.go")
	if err != nil {
		t.Fatal(err)
	}
	fromLines := map[int]bool{}
	for _, r := range res.Refs {
		if r.FromName == "Use" && r.ReferenceName == "String" {
			fromLines[r.FromLine] = true
		}
	}
	if !fromLines[5] || !fromLines[6] {
		t.Fatalf("Use→String refs must carry FromLine 5 AND 6 (enclosing method), got %v", fromLines)
	}
	if len(fromLines) != 2 {
		t.Fatalf("expected exactly 2 distinct FromLines, got %v", fromLines)
	}
}

func TestPromoteCallsToRefsEnclosingByRange(t *testing.T) {
	nodes := []ExtractedNode{
		{Kind: "method", Name: "String", File: "f.go", Line: 3, EndLine: 4},
		{Kind: "method", Name: "String", File: "f.go", Line: 9, EndLine: 10},
	}
	edges := []ExtractedEdge{
		{Kind: "calls", SourceName: "String", TargetName: "helper", Line: 4},
		{Kind: "calls", SourceName: "String", TargetName: "helper", Line: 10},
		{Kind: "calls", SourceName: "String", TargetName: "helper", Line: 15},
	}
	res := promoteCallsToRefs(nodes, edges, "f.go", "go")
	if len(res.Refs) != 3 {
		t.Fatalf("want 3 refs, got %d", len(res.Refs))
	}
	// Call at line 4 → first String (3-4); line 10 → second String (9-10);
	// line 15 → no containment, nearest def at/before 15 is line 9.
	want := []int{3, 9, 9}
	for i, r := range res.Refs {
		if r.FromLine != want[i] {
			t.Errorf("ref[%d].FromLine = %d, want %d", i, r.FromLine, want[i])
		}
	}
}

// ---- audit #2 (orchestrator): same-file calls to an ambiguous target must
// not first-wins-link to the wrong symbol; they park as unresolved. ----

func TestSameNameMethodsNoWrongEdge(t *testing.T) {
	dir := t.TempDir()
	src := `package p

func (a A) String() string { return "A" }
func (b B) String() string { return "B" }
func (a A) Use() string { return a.String() }
func (b B) Use() string { return b.String() }
`
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, dir)
	if _, err := orch.IndexFile(filepath.Join(dir, "a.go")); err != nil {
		t.Fatal(err)
	}

	uses, err := database.GetNodeByName("Use")
	if err != nil || len(uses) != 2 {
		t.Fatalf("expected 2 Use methods, got %d (err=%v)", len(uses), err)
	}
	// Neither Use method may carry a calls edge to a String method: the
	// target is ambiguous (two String methods) and the resolution pass must
	// refuse to guess instead of first-wins-linking.
	for _, u := range uses {
		callees, err := database.GetCalleesWithKind(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range callees {
			if c.Name == "String" && c.EdgeKind == db.EdgeCalls {
				t.Fatalf("Use(%d) wrongly linked to a String method: %+v", u.ID, callees)
			}
		}
	}
	// The ambiguous call refs could not resolve (tie) and are parked as
	// failed for a later retry, never linked to a wrong node.
	if n, _ := database.CountUnresolvedRefs("failed"); n == 0 {
		t.Fatal("expected failed unresolved refs for the ambiguous String calls")
	}
}

// ---- audit #3: C-like/Rust/Ruby/PHP call edges use the call-site line, not
// the enclosing function's definition line. ----

func TestTSCLikeCallSiteLines(t *testing.T) {
	// Line 1: #include
	// Line 2: (empty)
	// Line 3: void helper(void) {}
	// Line 5: void caller(void) {
	// Line 6:     helper();   <-- call site
	// Line 7:     helper();   <-- call site
	// Line 8: }
	source := `#include <stdio.h>

void helper(void) {}

void caller(void) {
    helper();
    helper();
}
`
	res, err := NewTreeSitterExtractor("c").Extract(source, "/a.c")
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "caller" && r.ReferenceName == "helper" {
			lines = append(lines, r.Line)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 call edges caller→helper, got %d (%v)", len(lines), lines)
	}
	has6, has7 := false, false
	for _, l := range lines {
		if l == 5 {
			t.Fatalf("call edge stamped with definition line 5: %v", lines)
		}
		if l == 6 {
			has6 = true
		}
		if l == 7 {
			has7 = true
		}
	}
	if !has6 || !has7 {
		t.Fatalf("expected call-site lines 6 and 7, got %v", lines)
	}
}

// ---- audit #5: regex fallback call edges use call-site lines too. ----

func TestRegexCallSiteLines(t *testing.T) {
	// Line 3: func caller() {
	// Line 4:     helper();
	// Line 5:     helper();
	source := `package p

func caller() {
	helper()
	helper()
}

func helper() {}
`
	res, err := NewExtractor("go").Extract(source, "/a.go")
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "caller" && r.ReferenceName == "helper" {
			lines = append(lines, r.Line)
		}
	}
	has4, has5 := false, false
	for _, l := range lines {
		if l == 3 {
			t.Fatalf("regex call edge has definition line 3: %v", lines)
		}
		if l == 4 {
			has4 = true
		}
		if l == 5 {
			has5 = true
		}
	}
	if !has4 || !has5 {
		t.Fatalf("expected regex call-site lines 4 and 5, got %v", lines)
	}
}

// ---- audit #6: findBraceEnd must ignore braces inside comments/strings and
// must not silently truncate functions longer than 500 lines. ----

func TestFindBraceEndSkipsCommentsAndStrings(t *testing.T) {
	lines := []string{
		"func foo() {",
		"\t// } not a brace",
		"\ts := \"}\"",
		"\t/* { also not a brace */",
		"\tif x { y() }",
		"}",
	}
	if got := findBraceEnd(lines, 0); got != 6 {
		t.Fatalf("findBraceEnd = %d, want 6 (comment/string braces must be ignored)", got)
	}
}

func TestFindBraceEndBeyond500Lines(t *testing.T) {
	lines := make([]string, 0, 520)
	lines = append(lines, "func foo() {")
	for i := 0; i < 510; i++ {
		lines = append(lines, "\tx := 1")
	}
	lines = append(lines, "}")
	if got := findBraceEnd(lines, 0); got != 512 {
		t.Fatalf("findBraceEnd = %d, want 512 (no silent 500-line truncation)", got)
	}
}

// ---- audit #10: Python regex body must survive a blank line / comment line
// right after the def header. ----

func TestFindIndentEndSkipsBlankAndComment(t *testing.T) {
	blank := []string{"def foo():", "", "    return 1"}
	if got := findIndentEnd(blank, 0); got != 3 {
		t.Fatalf("blank line after def: findIndentEnd = %d, want 3", got)
	}
	comment := []string{"def bar():", "    # only a comment", "    pass"}
	if got := findIndentEnd(comment, 0); got != 3 {
		t.Fatalf("comment line after def: findIndentEnd = %d, want 3", got)
	}
}

// ---- audit #14: Rust regex path must handle nested parens in fn params. ----

func TestRustFnNestedParenParams(t *testing.T) {
	source := `pub fn map(f: impl Fn(i32) -> i32) -> Vec<i32> {
	vec![]
}

fn with_ptr(cb: fn(i32, i32) -> i32) -> i32 {
	cb(1, 2)
}
`
	res, err := NewExtractor("rust").Extract(source, "/a.rs")
	if err != nil {
		t.Fatal(err)
	}
	mapFn := findNode(res.Nodes, "map")
	if mapFn == nil {
		t.Fatal("missing map fn (nested parens in params must not drop it)")
	}
	if !containsAll(mapFn.Signature, "(f: impl Fn(i32) -> i32)", "-> Vec") {
		t.Fatalf("map.Signature = %q", mapFn.Signature)
	}
	if mapFn.ReturnType != "Vec" {
		t.Fatalf("map.ReturnType = %q, want Vec", mapFn.ReturnType)
	}
	ptr := findNode(res.Nodes, "with_ptr")
	if ptr == nil {
		t.Fatal("missing with_ptr fn")
	}
	if !containsAll(ptr.Signature, "(cb: fn(i32, i32) -> i32)") {
		t.Fatalf("with_ptr.Signature = %q", ptr.Signature)
	}
}

// ---- audit #20: Go interface methods become signature nodes. ----

func TestTSGoInterfaceMethods(t *testing.T) {
	source := `package p

type Reader interface {
	Read(p []byte) (int, error)
	Close() error
}
`
	res, err := NewTreeSitterExtractor("go").Extract(source, "/reader.go")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*ExtractedNode{}
	for i := range res.Nodes {
		if res.Nodes[i].Kind == "signature" {
			byName[res.Nodes[i].Name] = &res.Nodes[i]
		}
	}
	read, ok := byName["Read"]
	if !ok {
		t.Fatal("interface method Read not extracted as signature node")
	}
	if read.QualifiedName != "Reader.Read" {
		t.Fatalf("Read.QualifiedName = %q, want Reader.Read", read.QualifiedName)
	}
	if close_, ok := byName["Close"]; !ok || close_.QualifiedName != "Reader.Close" {
		t.Fatalf("Close signature missing: %+v", byName)
	}
}

// ---- audit #19: BOM and CRLF sources must extract identically. ----

func TestNormalizeBOMAndCRLF(t *testing.T) {
	source := "\uFEFFpackage p\r\n\r\nfunc Hello() {}\r\n"
	res, err := NewExtractor("go").Extract(source, "/bom.go")
	if err != nil {
		t.Fatal(err)
	}
	if findNode(res.Nodes, "Hello") == nil {
		t.Fatalf("BOM/CRLF source lost Hello node: %+v", res.Nodes)
	}
	res2, err := NewTreeSitterExtractor("go").Extract(source, "/bom.go")
	if err != nil {
		t.Fatal(err)
	}
	if findNode(res2.Nodes, "Hello") == nil {
		t.Fatalf("tree-sitter BOM/CRLF source lost Hello node: %+v", res2.Nodes)
	}
}

// ---- audit #21: regex JS path extracts class methods and arrow functions. ----

func TestRegexJSClassMethodAndArrow(t *testing.T) {
	// helper() at line 3 is a BARE call inside render()'s body (no `return`
	// keyword to shield it). A class-body scan without a brace-depth check
	// misreads it as a sibling method; only the top-level function
	// declaration at the end may yield a "helper" node.
	source := `class Foo {
	render() {
		helper();
	}
}
const bar = (x) => x + 1;
function helper() { return 1 }
`
	res, err := NewExtractor("javascript").Extract(source, "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	render := findNode(res.Nodes, "render")
	if render == nil {
		t.Fatal("class method render missing (regex JS path)")
	}
	if render.Kind != "method" || render.QualifiedName != "Foo.render" {
		t.Fatalf("render meta: kind=%q qn=%q", render.Kind, render.QualifiedName)
	}
	for _, n := range res.Nodes {
		if n.Name == "helper" && (n.Kind == "method" || n.QualifiedName == "Foo.helper") {
			t.Fatalf("bare call helper() inside render() body became a method: kind=%q qn=%q line=%d", n.Kind, n.QualifiedName, n.Line)
		}
	}
	for _, e := range res.Edges {
		if e.Kind == "contains" && e.SourceName == "Foo" && e.TargetName == "helper" {
			t.Fatalf("contains edge Foo→helper must not exist (helper() is a call, not a method)")
		}
	}
	if findNode(res.Nodes, "bar") == nil {
		t.Fatal("arrow function bar missing (regex JS path)")
	}
	found := false
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "render" && r.ReferenceName == "helper" {
			found = true
			if r.Line != 3 {
				t.Fatalf("render→helper call line = %d, want 3", r.Line)
			}
		}
	}
	if !found {
		t.Fatal("render→helper call missing (regex JS path)")
	}
}

// ---- must-fix: tree-sitter parse timeout must keep a good old index instead
// of overwriting it with weak regex partial results (and must still write the
// fallback for files with no prior index). ----

func TestIndexFileTSTimeoutKeepsOldIndex(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	src := filepath.Join(root, "a.js")
	body := []byte("class Foo {\n  render() {\n    return 1\n  }\n}\n")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	orch := NewOrchestrator(database, root)
	if _, err := orch.IndexFile(src); err != nil {
		t.Fatal(err)
	}
	hs, _ := database.GetNodeByName("Foo")
	if len(hs) != 1 {
		t.Fatalf("setup: expected Foo indexed, got %d", len(hs))
	}

	// Content changes (passes the content-hash gate), then tree-sitter
	// times out while the regex fallback yields partial results. The old
	// tree-sitter index must be kept (ErrKeepOldIndex), the fallback must
	// NOT overwrite it, and the content hash must not be touched (M2: the
	// stale meta makes the next pass retry the file).
	changed := []byte("class Foo {\n  render() {\n    helper();\n  }\n}\n")
	if err := os.WriteFile(src, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	orch.extractFn = func(lang, source, store string) (ExtractResult, bool, error) {
		fb, _ := NewExtractor(lang).Extract(source, store)
		return fb, true, ErrTSParseTimeout
	}
	defer func() { orch.extractFn = nil }()

	n, err := orch.indexFile(src, "javascript", nil)
	if !errors.Is(err, ErrKeepOldIndex) {
		t.Fatalf("indexFile should return ErrKeepOldIndex on ts timeout, got: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 nodes on ts timeout, got %d", n)
	}
	hs, _ = database.GetNodeByName("Foo")
	if len(hs) != 1 {
		t.Fatalf("old tree-sitter index was overwritten by regex partial results: %d Foo nodes", len(hs))
	}
	same, herr := database.FileHasContentHash(db.StoragePath(root, src), hashContent(changed))
	if herr != nil || same {
		t.Fatalf("content_hash must NOT be touched on ts timeout: same=%v err=%v", same, herr)
	}
}

func TestIndexFileTSTimeoutNoOldIndexWritesFallback(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	src := filepath.Join(root, "a.js")
	body := []byte("function foo() { return 1 }\n")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	orch := NewOrchestrator(database, root)
	orch.extractFn = func(lang, source, store string) (ExtractResult, bool, error) {
		fb, _ := NewExtractor(lang).Extract(source, store)
		return fb, true, ErrTSParseTimeout
	}
	defer func() { orch.extractFn = nil }()

	// No previous index: the regex fallback is written so a brand-new file
	// still gets indexed on the first pass instead of failing forever.
	n, err := orch.indexFile(src, "javascript", nil)
	if err != nil {
		t.Fatalf("indexFile with no old index should write the fallback, got: %v", err)
	}
	if n == 0 {
		t.Fatal("expected fallback nodes written for a new file on ts timeout")
	}
	hs, _ := database.GetNodeByName("foo")
	if len(hs) != 1 {
		t.Fatalf("fallback result missing after ts timeout on new file: %d foo nodes", len(hs))
	}
}

// ---- helpers ----

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// ---- audit #15: tree-sitter Rust use flattening (brace groups, as-renames). ----

func TestTSRustUseFlattening(t *testing.T) {
	source := `use foo::{a, b::c};
use std::collections::HashMap as HM;

pub fn f() {}
`
	res, err := NewTreeSitterExtractor("rust").Extract(source, "/lib.rs")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == "imports" {
			got[e.TargetName] = true
		}
	}
	for _, want := range []string{"foo::a", "foo::b::c", "std::collections::HashMap"} {
		if !got[want] {
			t.Fatalf("rust use missing %q (edges=%v)", want, got)
		}
	}
}

// ---- audit #13: framework route handlers may be dotted names or method
// values; simplifyHandlerName reduces them to a bare symbol. ----

func TestDetectGoRoutesDottedAndMethodValueHandlers(t *testing.T) {
	src := `
r.GET("/users", handlers.listUsers)
r.POST("/users", (*User).Create)
chi.Get("/health", ctl.health)
`
	d := NewFrameworkDetector()
	routes := d.DetectRoutes(src, "/main.go", "go")
	if len(routes) < 3 {
		t.Fatalf("expected 3 routes, got %d: %+v", len(routes), routes)
	}
	byKey := map[string]string{}
	for _, r := range routes {
		byKey[r.Method+" "+r.Path] = simplifyHandlerName(r.Handler)
	}
	if byKey["GET /users"] != "listUsers" {
		t.Fatalf("dotted handler: %q", byKey["GET /users"])
	}
	if byKey["POST /users"] != "Create" {
		t.Fatalf("method value handler: %q", byKey["POST /users"])
	}
	if byKey["GET /health"] != "health" {
		t.Fatalf("chi dotted handler: %q", byKey["GET /health"])
	}
}

func TestDetectExpressDottedHandler(t *testing.T) {
	src := `app.get("/api/health", controllers.health.check)
`
	d := NewFrameworkDetector()
	routes := d.DetectRoutes(src, "/app.js", "javascript")
	if len(routes) != 1 {
		t.Fatalf("expected 1 express route, got %d: %+v", len(routes), routes)
	}
	if got := simplifyHandlerName(routes[0].Handler); got != "check" {
		t.Fatalf("express dotted handler: %q, want check", got)
	}
}

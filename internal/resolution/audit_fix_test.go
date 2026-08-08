package resolution_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/resolution"
)

// ---- audit #8: tsconfig without paths must fall through to jsconfig. ----

func TestTsconfigWithoutPathsFallsThroughToJsconfig(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearAliasCache()
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{
  "compilerOptions": { "target": "es2020" }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jsconfig.json"), []byte(`{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": { "@/*": ["src/*"] }
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	aliases := resolution.LoadProjectAliases(dir)
	if aliases == nil {
		t.Fatal("jsconfig paths must be picked up when tsconfig has no paths")
	}
	got := resolution.ApplyAliases("@/lib/utils", aliases, dir)
	if len(got) == 0 || got[0] != "src/lib/utils" {
		t.Fatalf("ApplyAliases(@/lib/utils) = %v, want [src/lib/utils]", got)
	}

	// When tsconfig later gains paths, tsconfig must win (mtime change detected).
	p := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(p, []byte(`{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": { "@t/*": ["tsrc/*"] }
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force a distinct mtime (same-ms rewrites can alias on coarse clocks).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	aliases2 := resolution.LoadProjectAliases(dir)
	if aliases2 == nil {
		t.Fatal("tsconfig paths must be found after the file gains paths")
	}
	got2 := resolution.ApplyAliases("@t/x", aliases2, dir)
	if len(got2) == 0 || got2[0] != "tsrc/x" {
		t.Fatalf("ApplyAliases(@t/x) = %v, want [tsrc/x]", got2)
	}
}

// ---- audit #8 (cache): a jsconfig added after a paths-less tsconfig was
// cached must be picked up (per-file cache entries). ----

func TestAliasCacheDetectsAddedJsconfig(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearAliasCache()
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{
  "compilerOptions": { "target": "es2020" }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if resolution.LoadProjectAliases(dir) != nil {
		t.Fatal("paths-less tsconfig must yield nil aliases")
	}
	// jsconfig with paths appears later — the cached tsconfig miss must not
	// shadow it forever.
	if err := os.WriteFile(filepath.Join(dir, "jsconfig.json"), []byte(`{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": { "@/*": ["src/*"] }
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	aliases := resolution.LoadProjectAliases(dir)
	if aliases == nil {
		t.Fatal("jsconfig added after tsconfig-nil cache must be discovered")
	}
	if got := resolution.ApplyAliases("@/x", aliases, dir); len(got) == 0 || got[0] != "src/x" {
		t.Fatalf("ApplyAliases(@/x) = %v, want [src/x]", got)
	}
}

// ---- audit #9: MatchName must reject ambiguous ties (two same-scoring
// candidates) instead of first-wins, and must not pick struct/interface as
// call targets. ----

func TestMatchNameAmbiguousTieRejected(t *testing.T) {
	// Two same-named functions in the same file: identical scores → refuse.
	cands := []db.Node{
		{ID: 1, Kind: db.KindFunction, Name: "String", File: "a.go", Line: 3, Body: "x"},
		{ID: 2, Kind: db.KindFunction, Name: "String", File: "a.go", Line: 6, Body: "y"},
	}
	if m := resolution.MatchName(cands, "String", "a.go", true); m.TargetID != 0 {
		t.Fatalf("ambiguous same-file tie must be rejected, got %+v", m)
	}

	// Two same-named functions in the same directory: identical scores → refuse.
	cands2 := []db.Node{
		{ID: 3, Kind: db.KindFunction, Name: "greet", File: "a/util.ts", Line: 3, Body: "x"},
		{ID: 4, Kind: db.KindFunction, Name: "greet", File: "a/other.ts", Line: 6, Body: "y"},
	}
	if m := resolution.MatchName(cands2, "greet", "a/main.ts", true); m.TargetID != 0 {
		t.Fatalf("ambiguous same-dir tie must be rejected, got %+v", m)
	}

	// Unique same-file function still resolves.
	solo := []db.Node{{ID: 5, Kind: db.KindFunction, Name: "Solo", File: "a.go", Line: 3, Body: "x"}}
	if m := resolution.MatchName(solo, "Solo", "a.go", true); m.TargetID == 0 {
		t.Fatal("unique same-file function must resolve")
	}

	// A struct alone is not a call target (preferCall): no edge.
	onlyStruct := []db.Node{{ID: 6, Kind: db.KindStruct, Name: "Foo", File: "b.go", Line: 3, Body: "x"}}
	if m := resolution.MatchName(onlyStruct, "Foo", "b.go", true); m.TargetID != 0 {
		t.Fatalf("struct must not be a call target, got %+v", m)
	}
	// ...but a class still is (Python/JS instantiation).
	onlyClass := []db.Node{{ID: 7, Kind: db.KindClass, Name: "Foo", File: "b.py", Line: 3, Body: "x"}}
	if m := resolution.MatchName(onlyClass, "Foo", "b.py", true); m.TargetID == 0 {
		t.Fatal("class must remain a call target for constructor-style calls")
	}

	// A function candidate always beats a same-proximity struct (the struct
	// is not a call target at all).
	mixed := []db.Node{
		{ID: 8, Kind: db.KindStruct, Name: "Foo", File: "a.go", Line: 3, Body: "x"},
		{ID: 9, Kind: db.KindFunction, Name: "Foo", File: "a.go", Line: 6, Body: "y"},
	}
	m := resolution.MatchName(mixed, "Foo", "a.go", true)
	if m.TargetID != 9 {
		t.Fatalf("function must win over struct as call target, got %+v", m)
	}
}

// ---- audit #11: the unique-callable fallback accepts a single callable in a
// DIRECT subdirectory of the caller's directory (documented semantics). ----

func TestUniqueCallableSubdirectoryAccepted(t *testing.T) {
	dir := t.TempDir()
	callerDir := filepath.Join(dir, "caller")
	if err := os.MkdirAll(callerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(callerDir, "main.go"), []byte(`package caller
func Caller() string { return Target() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(callerDir, "lib")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "target.go"), []byte(`package lib
func Target() string { return "ok" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	database := indexDir(t, dir)
	assertGraphCall(t, database, "Caller", "Target")
}

// ---- audit #12: workspace globs support multi-level patterns. ----

func TestWorkspaceGlobDeep(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearWorkspaceCache()
	writePkg := func(rel, name string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "package.json"), []byte(`{"name": "`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writePkg("packages/a", "@repo/a")
	writePkg("packages/b/c", "@repo/c")
	writePkg("apps/web/lib", "@repo/lib")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
  "name": "root",
  "workspaces": ["packages/**", "apps/*/lib"]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := resolution.LoadWorkspacePackages(dir)
	if ws == nil {
		t.Fatal("workspace packages nil")
	}
	if got := ws.ByName["@repo/a"]; got != "packages/a" {
		t.Fatalf("@repo/a = %q, want packages/a", got)
	}
	if got := ws.ByName["@repo/c"]; got != "packages/b/c" {
		t.Fatalf("@repo/c = %q, want packages/b/c (deep glob)", got)
	}
	if got := ws.ByName["@repo/lib"]; got != "apps/web/lib" {
		t.Fatalf("@repo/lib = %q, want apps/web/lib (multi-segment glob)", got)
	}
}

// ---- must-fix: packages/** must not descend into node_modules or hidden
// directories (their nested package.json files would pollute the workspace
// table and silently misroute imports). ----

func TestWorkspaceGlobSkipsNodeModulesAndHidden(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearWorkspaceCache()
	writePkg := func(rel, name string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "package.json"), []byte(`{"name": "`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writePkg("packages/a", "@repo/a")
	writePkg("packages/b/node_modules/dep", "dep-pkg")
	writePkg("packages/b/node_modules/@scope/x", "@scope/x")
	writePkg("packages/b/.git/objects", "fake-git")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
  "name": "root",
  "workspaces": ["packages/**"]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := resolution.LoadWorkspacePackages(dir)
	if ws == nil {
		t.Fatal("workspace packages nil")
	}
	if got := ws.ByName["@repo/a"]; got != "packages/a" {
		t.Fatalf("@repo/a = %q, want packages/a", got)
	}
	for name := range map[string]string{
		"dep-pkg":  "packages/b/node_modules/dep",
		"@scope/x": "packages/b/node_modules/@scope/x",
		"fake-git": "packages/b/.git/objects",
	} {
		if _, ok := ws.ByName[name]; ok {
			t.Fatalf("nested package %q under node_modules/.git must not enter the workspace table", name)
		}
	}
}

// ---- must-fix: infix "**" (apps/**/lib) must match lib directories at any
// depth under apps, not just the single-segment case. ----

func TestWorkspaceGlobInfixDoubleStar(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearWorkspaceCache()
	writePkg := func(rel, name string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "package.json"), []byte(`{"name": "`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writePkg("apps/web/lib", "@repo/web-lib")
	writePkg("apps/admin/deep/lib", "@repo/admin-lib")
	writePkg("apps/tools/nope", "@repo/nope")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
  "name": "root",
  "workspaces": ["apps/**/lib"]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := resolution.LoadWorkspacePackages(dir)
	if ws == nil {
		t.Fatal("workspace packages nil")
	}
	if got := ws.ByName["@repo/web-lib"]; got != "apps/web/lib" {
		t.Fatalf("@repo/web-lib = %q, want apps/web/lib (infix **)", got)
	}
	if got := ws.ByName["@repo/admin-lib"]; got != "apps/admin/deep/lib" {
		t.Fatalf("@repo/admin-lib = %q, want apps/admin/deep/lib (infix **, multiple levels)", got)
	}
	if _, ok := ws.ByName["@repo/nope"]; ok {
		t.Fatal("@repo/nope must not match apps/**/lib")
	}
}

// ---- must-fix: an out-of-workdir storage key must fail closed instead of
// resolving relative to the process CWD. ----

func TestResolveImportPathEscapingKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	// "../../escape" escapes workdir: db.AbsPath refuses it and returns "".
	// ResolveImportPath must return nil instead of resolving the spec against
	// filepath.Dir("") == "." (the test process CWD, where
	// ./audit_fix_test.go exists).
	got := resolution.ResolveImportPath(dir, "../../escape", "./audit_fix_test.go", "typescript")
	if len(got) != 0 {
		t.Fatalf("escaping storage key must fail closed, got %v", got)
	}
}

// ---- audit #7: event-emitter synthesis prefers a handler in the same file
// as the registration over a same-named handler elsewhere. ----

func TestEventEmitterHandlerSameFilePreferred(t *testing.T) {
	dir := t.TempDir()
	bus := `const Bus = require("events").EventEmitter
const bus = new Bus()
bus.on("mount", onmount)
function onmount() {}
export function publishMount() { bus.emit("mount") }
`
	other := `export function onmount() {}
`
	if err := os.WriteFile(filepath.Join(dir, "bus.js"), []byte(bus), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.js"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	database := indexDir(t, dir)

	pub, err := database.GetNodeByName("publishMount")
	if err != nil || len(pub) == 0 {
		t.Fatalf("publishMount missing: %v", err)
	}
	edges, err := database.GetOutgoingEdges(pub[0].ID, []string{db.EdgeCalls})
	if err != nil {
		t.Fatal(err)
	}
	var hits int
	for _, e := range edges {
		tgt, err := database.GetNodeByID(e.TargetID)
		if err != nil || tgt == nil || tgt.Name != "onmount" {
			continue
		}
		if tgt.File == "bus.js" {
			hits++
		}
	}
	if hits == 0 {
		t.Fatal("publishMount must synthesize event-emitter calls edge to the bus.js onmount handler")
	}
	// The other.js onmount must NOT be the target.
	for _, e := range edges {
		tgt, err := database.GetNodeByID(e.TargetID)
		if err != nil || tgt == nil {
			continue
		}
		if tgt.Name == "onmount" && tgt.File == "other.js" {
			t.Fatal("event-emitter must not cross-wire to a same-named handler in another file")
		}
	}
}

// ---- audit #22 (note): resolveOne's per-ref error path (CollectCandidates /
// writeEdge failing) now counts Failed and parks the ref as failed instead of
// logging and silently leaving it pending. The path is not injectable via the
// public API (FK constraints prevent dangling refs, and a working DB never
// errors on these queries), so it is verified by code review + the resolve
// suite; TestResolveAllIdempotent / TestFailedRefRetriesWhenTargetAppears pin
// the retry semantics that the change preserves.

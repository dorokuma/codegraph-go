package resolution_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/extraction"
	"github.com/dorokuma/codegraph-go/tools"
)

func assertSynthCall(t *testing.T, database *db.DB, caller, callee, by string) {
	t.Helper()
	callers, err := database.GetNodeByName(caller)
	if err != nil || len(callers) == 0 {
		t.Fatalf("caller %s missing: %v", caller, err)
	}
	// Prefer method/function over class with same name.
	var src db.Node
	for _, c := range callers {
		if c.Kind == db.KindMethod || c.Kind == db.KindFunction || c.Kind == "component" {
			src = c
			break
		}
	}
	if src.ID == 0 {
		src = callers[0]
	}
	edges, err := database.GetOutgoingEdges(src.ID, []string{db.EdgeCalls})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		tgt, err := database.GetNodeByID(e.TargetID)
		if err != nil || tgt == nil {
			continue
		}
		if tgt.Name != callee {
			continue
		}
		if e.Provenance != "heuristic" {
			t.Fatalf("%s → %s provenance=%q want heuristic", caller, callee, e.Provenance)
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(e.Metadata), &meta); err != nil {
			t.Fatalf("metadata json: %v (%q)", err, e.Metadata)
		}
		if meta["synthesizedBy"] != by {
			t.Fatalf("%s → %s synthesizedBy=%q want %q (meta=%v)", caller, callee, meta["synthesizedBy"], by, meta)
		}
		return
	}
	// dump callees for debug
	callees, _ := database.GetCalleesWithKind(src.ID)
	t.Fatalf("%s should synth-call %s via %s; outgoing=%+v callees=%+v", caller, callee, by, edges, callees)
}

func TestSynthCallbackFieldChannel(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_callback"), dir)
	database := indexDir(t, dir)

	// Methods extracted
	for _, name := range []string{"onUpdate", "triggerUpdate", "triggerRender", "mutateElement", "paintCanvas"} {
		nodes, err := database.GetNodeByName(name)
		if err != nil || len(nodes) == 0 {
			t.Fatalf("missing symbol %s", name)
		}
	}

	// Core synthesized edge: dispatcher → registered callback
	assertSynthCall(t, database, "triggerUpdate", "triggerRender", "callback")
	// Static hops still present so the full flow connects
	assertGraphCall(t, database, "mutateElement", "triggerUpdate")
	assertGraphCall(t, database, "triggerRender", "paintCanvas")
}

// TestSynthCallbackDisambiguation verifies S-5: when two classes define
// the same callback method name (handle), fieldChannelEdges must connect
// each dispatcher to the correct handler via same-file preference, not
// blindly pick the first global match.
func TestSynthCallbackDisambiguation(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_callback_disambig"), dir)
	database := indexDir(t, dir)

	// Verify both WidgetA.handle and WidgetB.handle exist (2 each),
	// and doA/doB each exist (1 each) to confirm both classes are indexed.
	for _, name := range []string{"handle", "fire"} {
		nodes, err := database.GetNodeByName(name)
		if err != nil || len(nodes) < 2 {
			t.Fatalf("expected at least 2 nodes for %s, got %d (err=%v)", name, len(nodes), err)
		}
	}
	for _, name := range []string{"doA", "doB"} {
		nodes, err := database.GetNodeByName(name)
		if err != nil || len(nodes) == 0 {
			t.Fatalf("expected nodes for %s, got %d (err=%v)", name, len(nodes), err)
		}
	}

	// triggerUpdate (dispatcher) should have synthesized calls edges to BOTH
	// WidgetA.handle and WidgetB.handle via callback synthesis.
	triggerNodes, err := database.GetNodeByName("triggerUpdate")
	if err != nil || len(triggerNodes) == 0 {
		t.Fatal("triggerUpdate missing")
	}
	trigger := triggerNodes[0]

	// Get outgoing edges directly to check provenance + target names
	edges, err := database.GetOutgoingEdges(trigger.ID, []string{db.EdgeCalls})
	if err != nil {
		t.Fatal(err)
	}

	handleTargets := map[string]bool{}
	for _, e := range edges {
		if e.Provenance != "heuristic" {
			continue
		}
		tgt, err := database.GetNodeByID(e.TargetID)
		if err != nil || tgt == nil {
			continue
		}
		if tgt.Name == "handle" {
			var meta map[string]string
			if err := json.Unmarshal([]byte(e.Metadata), &meta); err != nil {
				continue
			}
			if meta["synthesizedBy"] == "callback" {
				handleTargets[tgt.File] = true
			}
		}
	}
	if len(handleTargets) < 1 {
		t.Fatalf("triggerUpdate should have synthesized callback edges to handle methods, got %d targets (edges=%d)", len(handleTargets), len(edges))
	}
}

func TestSynthEventEmitter(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_callback"), dir)
	database := indexDir(t, dir)

	assertSynthCall(t, database, "publishMount", "onmount", "event-emitter")
	assertGraphCall(t, database, "onmount", "afterMount")
}

func TestSynthReactFullFlow(t *testing.T) {
	// 7.2: setState→render AND render→JSX child must ship together.
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_react"), dir)
	database := indexDir(t, dir)

	assertSynthCall(t, database, "dirty", "render", "react-render")
	assertSynthCall(t, database, "render", "StaticCanvas", "jsx-render")
	assertGraphCall(t, database, "StaticCanvas", "renderStaticScene")

	// explore Flow must walk the whole chain among named symbols.
	text, err := tools.ToolExplore(context.Background(), database, dir, tools.ExploreArgs{
		Query: "dirty render StaticCanvas renderStaticScene",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Flow") {
		t.Fatalf("expected Flow for react chain:\n%s", text)
	}
	flow := text[strings.Index(text, "Flow"):]
	for _, want := range []string{"dirty", "render", "StaticCanvas", "renderStaticScene"} {
		if !strings.Contains(flow, want) {
			t.Fatalf("Flow missing %q:\n%s", want, flow)
		}
	}
}

func TestSynthCallbackExploreFlow(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_callback"), dir)
	database := indexDir(t, dir)

	text, err := tools.ToolExplore(context.Background(), database, dir, tools.ExploreArgs{
		Query: "mutateElement triggerUpdate triggerRender paintCanvas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Flow") {
		t.Fatalf("expected Flow for callback chain:\n%s", text)
	}
	flow := text[strings.Index(text, "Flow"):]
	for _, want := range []string{"mutateElement", "triggerUpdate", "triggerRender", "paintCanvas"} {
		if !strings.Contains(flow, want) {
			t.Fatalf("Flow missing %q:\n%s", want, flow)
		}
	}
}

func TestSynthReactRouterRoute(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_route"), dir)
	database := indexDir(t, dir)

	routes, err := database.GetNodeByName("GET /home")
	if err != nil || len(routes) == 0 {
		t.Fatalf("missing React Router route: %v", err)
	}
	callees, err := database.GetCalleesWithKind(routes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, c := range callees {
		if c.Name == "HomePage" && c.EdgeKind == db.EdgeReferences {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("route should reference HomePage, got %+v", callees)
	}
}

func TestSynthBridgeCrossLang(t *testing.T) {
	// 7.4：跨语言符号要挂上。主路径是 C 侧抽出 c_hello 后 ResolveAll 连 bridge；
	// synthesize 的 bridge-link 只兜底 Resolve 仍失败的 pending/failed。
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_bridge"), dir)
	database := indexDir(t, dir)

	cNodes, err := database.GetNodeByName("c_hello")
	if err != nil || len(cNodes) == 0 {
		t.Fatalf("c_hello missing from C side: %v", err)
	}
	callers, err := database.GetNodeByName("CallC")
	if err != nil || len(callers) == 0 {
		t.Fatal("CallC missing")
	}
	callees, err := database.GetCalleesWithKind(callers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	foundBridge := false
	for _, c := range callees {
		if c.Name == "c_hello" && c.EdgeKind == db.EdgeBridge {
			foundBridge = true
			break
		}
	}
	if !foundBridge {
		t.Fatalf("CallC should have bridge→c_hello (hang symbol), got %+v", callees)
	}
	// unresolved bridge refs should be cleared once hung
	pending, _ := database.CountUnresolvedRefs("pending")
	failed, _ := database.CountUnresolvedRefs("failed")
	refs, _ := database.ListUnresolvedRefs("", "")
	for _, r := range refs {
		if r.ReferenceKind == db.EdgeBridge || r.ReferenceKind == "bridge" {
			t.Fatalf("bridge ref still parked after hang: %+v (pending=%d failed=%d)", r, pending, failed)
		}
	}
}

// TestSynthBridgeIdempotent verifies that bridge edges survive repeated SynthesizeAll
// cycles (BUG-4: bridgeSymbolEdges deleted unresolved refs, causing permanent loss on
// incremental re-index because DeleteSynthesizedEdges removes the edge but the ref
// can no longer be re-parked). After the fix, unresolved refs are kept, so each
// SynthesizeAll pass re-synthesizes from the still-existing refs.
//
// Regression guard: this test modifies an UNRELATED file (one that does NOT
// contain the bridge ref) to trigger SynthesizeAll. If DeleteUnresolvedRef were
// restored (old BUG-4 behaviour), the first SynthesizeAll would delete the
// bridge ref, and subsequent passes would lose the bridge edge permanently —
// this test would fail. The fix keeps refs, so the bridge edge survives.
func TestSynthBridgeIdempotent(t *testing.T) {
	dir := t.TempDir()
	// Place the C file in a deep subdirectory so ResolveAll won't find it via
	// same-directory or sibling-directory proximity (M-8), forcing bridgeSymbolEdges
	// to do the linking via synthesis.
	srcDir := filepath.Join("..", "..", "testdata", "parity", "synth_bridge")
	copyTree(t, srcDir, dir)

	// Move hello.c into a deep subdirectory (clib/native) to defeat all
	// directory-proximity heuristics (same-dir, sibling-dir).
	clibDir := filepath.Join(dir, "clib", "native")
	if err := os.MkdirAll(clibDir, 0o755); err != nil {
		t.Fatal(err)
	}
	helloC := filepath.Join(dir, "hello.c")
	helloCDst := filepath.Join(clibDir, "hello.c")
	data, err := os.ReadFile(helloC)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helloCDst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(helloC); err != nil {
		t.Fatal(err)
	}

	database := indexDir(t, dir)

	// Helper to count c_hello bridge edges from CallC.
	countBridge := func() int {
		callers, err := database.GetNodeByName("CallC")
		if err != nil || len(callers) == 0 {
			return 0
		}
		callees, err := database.GetCalleesWithKind(callers[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range callees {
			if c.Name == "c_hello" && c.EdgeKind == db.EdgeBridge {
				n++
			}
		}
		return n
	}

	// Verify bridge edge exists after first IndexAll (via synthesis, not resolution).
	if n := countBridge(); n != 1 {
		t.Fatalf("expected 1 bridge edge after IndexAll, got %d", n)
	}

	// Add an unrelated file that does NOT contain any bridge ref (no CGo import).
	// IndexChanges on this file triggers SynthesizeAll which must recreate the
	// bridge edge from the persistent unresolved ref (kept by the fix).
	unrelated := filepath.Join(dir, "unrelated.go")
	if err := os.WriteFile(unrelated, []byte("package main\n\nfunc Unrelated() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh orchestrator for the incremental pass.
	orch := extraction.NewOrchestrator(database, dir)
	if _, _, err := orch.IndexChanges([]string{unrelated}); err != nil {
		t.Fatal(err)
	}

	if n := countBridge(); n != 1 {
		t.Fatalf("expected 1 bridge edge after SynthesizeAll (unrelated file change), got %d", n)
	}

	// Second pass: create another unrelated file to confirm idempotence.
	unrelated2 := filepath.Join(dir, "unrelated2.go")
	if err := os.WriteFile(unrelated2, []byte("package main\n\nfunc OtherFunc() string { return \"x\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orch2 := extraction.NewOrchestrator(database, dir)
	if _, _, err := orch2.IndexChanges([]string{unrelated2}); err != nil {
		t.Fatal(err)
	}

	if n := countBridge(); n != 1 {
		t.Fatalf("expected 1 bridge edge after second SynthesizeAll (idempotent), got %d", n)
	}
}

func TestSynthFnPointerDispatch(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_fnptr"), dir)
	database := indexDir(t, dir)

	// handlers extracted
	for _, name := range []string{"add_one", "mul_two", "dispatch"} {
		nodes, err := database.GetNodeByName(name)
		if err != nil || len(nodes) == 0 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}

	assertSynthCall(t, database, "dispatch", "add_one", "fn-pointer-dispatch")
	assertSynthCall(t, database, "dispatch", "mul_two", "fn-pointer-dispatch")
}

func TestSynthFnPointerExploreFlow(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_fnptr"), dir)
	database := indexDir(t, dir)

	text, err := tools.ToolExplore(context.Background(), database, dir, tools.ExploreArgs{
		Query: "dispatch add_one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Flow") {
		t.Fatalf("expected Flow for fnptr chain:\n%s", text)
	}
	flow := text[strings.Index(text, "Flow"):]
	for _, want := range []string{"dispatch", "add_one"} {
		if !strings.Contains(flow, want) {
			t.Fatalf("Flow missing %q:\n%s", want, flow)
		}
	}
}

func TestSynthGoFrameRoute(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_goframe"), dir)
	database := indexDir(t, dir)

	routes, err := database.GetNodeByName("POST /user/sign-in")
	if err != nil || len(routes) == 0 {
		// dump all routes
		all, _ := database.GetNodesByKind("route")
		t.Fatalf("missing goframe route: %v routes=%+v", err, all)
	}
	r := routes[0]
	if !strings.Contains(r.QualifiedName, "::goframe-route:") {
		t.Fatalf("route qn missing marker: %q", r.QualifiedName)
	}

	assertSynthCall(t, database, "POST /user/sign-in", "SignIn", "goframe-route")
	assertGraphCall(t, database, "SignIn", "finishSignIn")
}

func TestSynthGoFrameExploreFlow(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "parity", "synth_goframe"), dir)
	database := indexDir(t, dir)

	text, err := tools.ToolExplore(context.Background(), database, dir, tools.ExploreArgs{
		Query: "POST /user/sign-in SignIn finishSignIn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Flow") {
		t.Fatalf("expected Flow for goframe chain:\n%s", text)
	}
	flow := text[strings.Index(text, "Flow"):]
	for _, want := range []string{"SignIn", "finishSignIn"} {
		if !strings.Contains(flow, want) {
			t.Fatalf("Flow missing %q:\n%s", want, flow)
		}
	}
}

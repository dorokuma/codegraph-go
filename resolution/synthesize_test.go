package resolution_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
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
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_callback"), dir)
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

func TestSynthEventEmitter(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_callback"), dir)
	database := indexDir(t, dir)

	assertSynthCall(t, database, "publishMount", "onmount", "event-emitter")
	assertGraphCall(t, database, "onmount", "afterMount")
}

func TestSynthReactFullFlow(t *testing.T) {
	// 7.2: setState→render AND render→JSX child must ship together.
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_react"), dir)
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
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_callback"), dir)
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
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_route"), dir)
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
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_bridge"), dir)
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


func TestSynthFnPointerDispatch(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_fnptr"), dir)
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
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_fnptr"), dir)
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
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_goframe"), dir)
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
	copyTree(t, filepath.Join("..", "testdata", "parity", "synth_goframe"), dir)
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

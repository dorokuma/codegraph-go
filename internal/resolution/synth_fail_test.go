package resolution

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestSynthesisCancellationPreservesCompleteEdges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var active atomic.Bool
	var reads atomic.Int32
	function := registerCancelLookup(t, func(name string) {
		if active.Load() && name == "render" {
			reads.Add(1)
			cancel() // inside classMethods' GetNodeByID, not after a pass
		}
	})
	database, caller, target, _ := newBatchTestDB(t, 0)
	cls, err := database.UpsertNode(&db.Node{Kind: db.KindClass, Name: "Widget", File: "a.tsx", Line: 1, EndLine: 20})
	if err != nil {
		t.Fatal(err)
	}
	render, err := database.UpsertNode(&db.Node{Kind: db.KindMethod, Name: "render", File: "a.tsx", Line: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertEdge(&db.Edge{SourceID: cls, TargetID: render, Kind: db.EdgeContains}); err != nil {
		t.Fatal(err)
	}
	const metadata = `{"synthesizedBy":"keep-me"}`
	if _, err := database.UpsertEdge(&db.Edge{
		SourceID: caller, TargetID: target, Kind: db.EdgeCalls,
		File: "a.go", Line: 1, Provenance: ProvHeuristic, Metadata: metadata,
	}); err != nil {
		t.Fatal(err)
	}
	restore := installCancelLookup(t, database, function)
	passes := 0
	oldHook := failSynthPass
	failSynthPass = func(name string) error {
		passes++
		if name == "event-emitter" {
			active.Store(true) // arm the real read in the next pass
		}
		return nil
	}
	t.Cleanup(func() { failSynthPass = oldHook })
	st, err := SynthesizeAllContext(ctx, database, t.TempDir())
	if !errors.Is(err, context.Canceled) || st.Written != 0 || passes != 2 || reads.Load() != 1 {
		t.Fatalf("stats=%+v passes=%d reads=%d err=%v", st, passes, reads.Load(), err)
	}
	restore()
	edges, err := database.GetOutgoingEdges(caller, []string{db.EdgeCalls})
	if err != nil || len(edges) != 1 || edges[0].Metadata != metadata {
		t.Fatalf("canceled synthesis replaced the previous graph: edges=%v err=%v", edges, err)
	}
}

func TestSynthesisPassesCheckCancellation(t *testing.T) {
	database, _, _, _ := newBatchTestDB(t, 0)
	ctx, err := newSynthCtx(database, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx.cancelCtx = cancelCtx
	ctx.allFiles = []string{"a.c", "a.tsx"}
	for name, pass := range map[string]func(*synthCtx) ([]synthEdge, error){
		"callback": fieldChannelEdges, "event-emitter": eventEmitterEdges,
		"react-render": reactRenderEdges, "jsx-render": reactJSXChildEdges,
		"bridge-link": bridgeSymbolEdges, "fn-pointer": cFnPointerDispatchEdges,
		"goframe": goframeRouteEdges,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pass(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("pass ignored cancellation: %v", err)
			}
		})
	}
}

func TestSynthesizeAllPassErrorSkipsReplace(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	id, err := database.UpsertNode(&db.Node{
		Kind: db.KindFunction, Name: "A", File: "a.go", Line: 1, Language: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertFileRecord(&db.FileRecord{Path: "a.go", Language: "go", NodeCount: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertEdge(&db.Edge{
		SourceID: id, TargetID: id, Kind: db.EdgeCalls,
		File: "a.go", Line: 1, Provenance: ProvHeuristic,
		Metadata: `{"synthesizedBy":"keep-me"}`,
	}); err != nil {
		t.Fatal(err)
	}

	failSynthPass = func(name string) error {
		if name == "callback" {
			return errors.New("forced pass failure")
		}
		return nil
	}
	t.Cleanup(func() { failSynthPass = nil })

	if _, err := SynthesizeAll(database, dir); err == nil {
		t.Fatal("expected pass error")
	}

	edges, err := database.GetOutgoingEdges(id, []string{db.EdgeCalls})
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, e := range edges {
		if e.Provenance == ProvHeuristic && e.Metadata == `{"synthesizedBy":"keep-me"}` {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("partial synth failure must not replace existing heuristic edges: %+v", edges)
	}
}

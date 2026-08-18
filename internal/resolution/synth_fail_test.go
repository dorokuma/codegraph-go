package resolution

import (
	"errors"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

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

package db

import "testing"

func TestNeedsRebuildFresh(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	need, old, err := database.NeedsRebuild()
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatalf("fresh db should need rebuild, old=%s", old)
	}
	if err := database.SetLogicVersion(); err != nil {
		t.Fatal(err)
	}
	need, _, err = database.NeedsRebuild()
	if err != nil || need {
		t.Fatalf("after set, need=%v err=%v", need, err)
	}
}

func TestWipeIndex(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	id, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "f", File: "/a.go", Line: 1})
	if err != nil || id == 0 {
		t.Fatalf("upsert %v %d", err, id)
	}
	if err := database.WipeIndex(); err != nil {
		t.Fatal(err)
	}
	nodes, err := database.GetNodeByName("f")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("expected empty after wipe, got %d", len(nodes))
	}
}

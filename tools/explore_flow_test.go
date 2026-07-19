package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
)

func TestGetExploreOutputBudgetMonotonic(t *testing.T) {
	small := GetExploreOutputBudget(100)
	mid := GetExploreOutputBudget(1000)
	huge := GetExploreOutputBudget(30000)

	if small.MaxOutputChars >= huge.MaxOutputChars {
		t.Fatalf("small total cap should be < huge: %d vs %d", small.MaxOutputChars, huge.MaxOutputChars)
	}
	if small.DefaultMaxFiles >= huge.DefaultMaxFiles {
		t.Fatalf("small maxFiles should be < huge: %d vs %d", small.DefaultMaxFiles, huge.DefaultMaxFiles)
	}
	if small.MaxCharsPerFile > mid.MaxCharsPerFile || mid.MaxCharsPerFile > huge.MaxCharsPerFile {
		t.Fatalf("per-file caps must be non-decreasing: %d → %d → %d",
			small.MaxCharsPerFile, mid.MaxCharsPerFile, huge.MaxCharsPerFile)
	}
	if small.MaxOutputChars > 20000 {
		t.Fatalf("small projects must stay under ~20k chars, got %d", small.MaxOutputChars)
	}
	if mid.MaxOutputChars > 24000 || huge.MaxOutputChars > 24000 {
		t.Fatalf("large tiers must stay at inline ceiling ≤24k, got mid=%d huge=%d", mid.MaxOutputChars, huge.MaxOutputChars)
	}
	// Tier boundaries do not invert.
	if GetExploreOutputBudget(149).MaxOutputChars != small.MaxOutputChars {
		t.Fatal("<150 boundary drift")
	}
	if GetExploreOutputBudget(499).MaxOutputChars != GetExploreOutputBudget(200).MaxOutputChars {
		t.Fatal("<500 boundary drift")
	}
	if GetExploreOutputBudget(14999).MaxOutputChars != GetExploreOutputBudget(10000).MaxOutputChars {
		t.Fatal("<15000 boundary drift")
	}
}

func TestGetExploreBudgetCalls(t *testing.T) {
	if GetExploreBudget(100) != 1 || GetExploreBudget(1000) != 2 || GetExploreBudget(10000) != 3 {
		t.Fatalf("unexpected call budget: %d %d %d", GetExploreBudget(100), GetExploreBudget(1000), GetExploreBudget(10000))
	}
	if GetExploreBudget(20000) != 4 || GetExploreBudget(50000) != 5 {
		t.Fatalf("large call budget wrong")
	}
}

func TestExploreFlowTwoSymbols(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	// foo → bar is a direct calls edge; bag query must surface Flow.
	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Query: "foo bar"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Flow") {
		t.Fatalf("expected Flow section for two related symbols, got:\n%s", text)
	}
	if !strings.Contains(text, "foo") || !strings.Contains(text, "bar") {
		t.Fatalf("flow missing endpoints:\n%s", text)
	}
	// Source still present and marked already-read.
	if !strings.Contains(text, "func foo") && !strings.Contains(text, "func bar") {
		t.Fatalf("expected source bodies:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "use the read tool") {
		t.Fatalf("must not push external Read:\n%s", text)
	}
}

func TestExploreFlowThreeHop(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Query: "main foo bar"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Flow") {
		t.Fatalf("expected Flow:\n%s", text)
	}
	// Find within the Flow block specifically
	flowIdx := strings.Index(text, "Flow")
	if flowIdx < 0 {
		t.Fatal("no Flow")
	}
	flowPart := text[flowIdx:]
	if end := strings.Index(flowPart, "## "); end > 0 {
		flowPart = flowPart[:end]
	}
	iMain := strings.Index(flowPart, "main")
	iFoo := strings.Index(flowPart, "foo")
	iBar := strings.Index(flowPart, "bar")
	if iMain < 0 || iFoo < 0 || iBar < 0 || !(iMain < iFoo && iFoo < iBar) {
		t.Fatalf("expected main→foo→bar order in Flow, got:\n%s", flowPart)
	}
}

func TestExploreFlowBridgeOneUnnamed(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	c := filepath.Join(dir, "c.go")
	// A → bridge → C; agent names A and C only.
	idA, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "AlphaEntry", File: a, Line: 1, Body: "func AlphaEntry() {}", Language: "go"})
	idB, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "hiddenBridge", File: b, Line: 1, Body: "func hiddenBridge() {}", Language: "go"})
	idC, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "OmegaEnd", File: c, Line: 1, Body: "func OmegaEnd() {}", Language: "go"})
	database.UpsertEdge(&db.Edge{SourceID: idA, TargetID: idB, Kind: db.EdgeCalls, File: a, Line: 2})
	database.UpsertEdge(&db.Edge{SourceID: idB, TargetID: idC, Kind: db.EdgeCalls, File: b, Line: 2})
	database.UpsertFile(a, 10, 1)
	database.UpsertFile(b, 10, 1)
	database.UpsertFile(c, 10, 1)

	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Query: "AlphaEntry OmegaEnd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Flow") {
		t.Fatalf("expected Flow via one unnamed bridge:\n%s", text)
	}
	if !strings.Contains(text, "hiddenBridge") {
		t.Fatalf("bridge hop should appear on spine:\n%s", text)
	}
}

func TestExploreFlowNoEdgeNoFlow(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	f1 := filepath.Join(dir, "x.go")
	f2 := filepath.Join(dir, "y.go")
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "AloneOne", File: f1, Line: 1, Body: "func AloneOne() {}", Language: "go"})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "AloneTwo", File: f2, Line: 1, Body: "func AloneTwo() {}", Language: "go"})
	database.UpsertFile(f1, 10, 1)
	database.UpsertFile(f2, 10, 1)

	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Query: "AloneOne AloneTwo"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "**Flow") {
		t.Fatalf("unrelated symbols must not invent a Flow:\n%s", text)
	}
}

func TestTokenizeExploreQuery(t *testing.T) {
	toks := tokenizeExploreQuery("PmsProductController getList Service.list foo.go")
	joined := strings.Join(toks, ",")
	if !strings.Contains(joined, "PmsProductController") || !strings.Contains(joined, "getList") {
		t.Fatalf("tokens=%v", toks)
	}
	// file extension stripped
	for _, tkn := range toks {
		if strings.HasSuffix(strings.ToLower(tkn), ".go") {
			t.Fatalf("ext not stripped: %v", toks)
		}
	}
}

func TestExploreSingleSymbolStillHasSource(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Query: "foo", Max: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "func foo") {
		t.Fatalf("single-symbol explore must still return body:\n%s", text)
	}
}

func TestExploreFlowRespectsPath(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	projA := filepath.Join(dir, "projA")
	projB := filepath.Join(dir, "projB")
	os.MkdirAll(projA, 0o755)
	os.MkdirAll(projB, 0o755)

	a1 := filepath.Join(projA, "a.go")
	a2 := filepath.Join(projA, "b.go")
	id1, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "StartFlow", File: a1, Line: 1, Body: "func StartFlow() {}", Language: "go"})
	id2, _ := database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "EndFlow", File: a2, Line: 1, Body: "func EndFlow() {}", Language: "go"})
	database.UpsertEdge(&db.Edge{SourceID: id1, TargetID: id2, Kind: db.EdgeCalls, File: a1, Line: 2})
	database.UpsertFile(a1, 10, 1)
	database.UpsertFile(a2, 10, 1)

	b1 := filepath.Join(projB, "a.go")
	b2 := filepath.Join(projB, "b.go")
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "StartFlow", File: b1, Line: 1, Body: "func StartFlow() { /*B*/ }", Language: "go"})
	database.UpsertNode(&db.Node{Kind: db.KindFunction, Name: "EndFlow", File: b2, Line: 1, Body: "func EndFlow() { /*B*/ }", Language: "go"})
	database.UpsertFile(b1, 10, 1)
	database.UpsertFile(b2, 10, 1)

	// Edge only exists under projA; path=projB must not paint a projA Flow.
	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{
		Query: "StartFlow EndFlow",
		Path:  "projB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "projA") {
		t.Fatalf("path=projB must not surface projA in Flow/source:\n%s", text)
	}
	if strings.Contains(text, "**Flow") {
		t.Fatalf("projB has no edge — must not invent Flow from projA:\n%s", text)
	}
	if !strings.Contains(text, "/*B*/") {
		t.Fatalf("expected projB bodies:\n%s", text)
	}

	// path=projA should show the real chain, still without projB.
	textA, err := ToolExplore(context.Background(), database, dir, ExploreArgs{
		Query: "StartFlow EndFlow",
		Path:  "projA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textA, "Flow") || !strings.Contains(textA, "projA") {
		t.Fatalf("path=projA should Flow inside projA:\n%s", textA)
	}
	if strings.Contains(textA, "projB") || strings.Contains(textA, "/*B*/") {
		t.Fatalf("path=projA must not bleed projB:\n%s", textA)
	}
}

func TestExploreMaxHonored(t *testing.T) {
	database, dir, cleanup := seedGraph(t)
	defer cleanup()

	// Explicit Max above tier default must not be silently clamped to 4/5.
	text, err := ToolExplore(context.Background(), database, dir, ExploreArgs{
		Query: "main foo bar",
		Max:   40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "across up to 40 files") {
		t.Fatalf("explicit Max=40 should be honored, got:\n%s", text)
	}

	// Zero Max → tier default (tiny repo → 4).
	text0, err := ToolExplore(context.Background(), database, dir, ExploreArgs{Query: "foo bar"})
	if err != nil {
		t.Fatal(err)
	}
	defaultFiles := GetExploreOutputBudget(3).DefaultMaxFiles
	want := fmt.Sprintf("across up to %d files", defaultFiles)
	if !strings.Contains(text0, want) {
		t.Fatalf("Max=0 should use tier default %q, got:\n%s", want, text0)
	}
}

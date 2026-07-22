package extraction

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func setupIndexDir(t *testing.T) (dir string, database *db.DB, cleanup func()) {
	t.Helper()
	dir = t.TempDir()
	cg := filepath.Join(dir, ".codegraph")
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(cg, "codegraph.db"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, database, func() { database.Close() }
}

func write(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestE2EMetadataInDB(t *testing.T) {
	dir, database, cleanup := setupIndexDir(t)
	defer cleanup()
	write(t, dir, "pkg.go", `package app

func Hello(name string) error { return nil }

type S struct{}

func (s *S) Method(x int) string { return "ok" }
`)
	o := NewOrchestrator(database, dir)
	o.SetForceReindex(true)
	t.Setenv("CODEGRAPH_INDEX_WORKERS", "1")
	if _, _, err := o.IndexAll(); err != nil {
		t.Fatal(err)
	}

	hs, err := database.GetNodeByName("Hello")
	if err != nil || len(hs) == 0 {
		t.Fatalf("Hello: %v %+v", err, hs)
	}
	h := hs[0]
	if h.QualifiedName != "app.Hello" || !h.IsExported || !strings.Contains(h.Signature, "(name string)") || h.ReturnType != "error" {
		t.Fatalf("Hello metadata: qn=%q exp=%v sig=%q ret=%q", h.QualifiedName, h.IsExported, h.Signature, h.ReturnType)
	}
	ms, _ := database.GetNodeByName("Method")
	if len(ms) == 0 || ms[0].QualifiedName != "S.Method" {
		t.Fatalf("Method qn: %+v", ms)
	}
	// contains edge present in DB
	ss, _ := database.GetNodeByName("S")
	if len(ss) == 0 {
		t.Fatal("missing S")
	}
	outs, _ := database.GetOutgoingEdges(ss[0].ID, []string{db.EdgeContains})
	if len(outs) == 0 {
		t.Fatal("missing contains edges from S")
	}
}

func TestE2EGoCloseSameFileLinks(t *testing.T) {
	// User-defined close must extract + same-file link (not killed as keyword).
	src := `package p
func close() {}
func main() { close() }
`
	res := NewTreeSitterExtractor("go").Extract(src, "a.go")
	has := false
	for _, r := range res.Refs {
		if r.FromName == "main" && r.ReferenceName == "close" {
			has = true
		}
	}
	if !has {
		t.Fatalf("close() call missing from refs: %+v", res.Refs)
	}

	dir, database, cleanup := setupIndexDir(t)
	defer cleanup()
	write(t, dir, "a.go", src)
	o := NewOrchestrator(database, dir)
	o.SetForceReindex(true)
	t.Setenv("CODEGRAPH_INDEX_WORKERS", "1")
	if _, _, err := o.IndexAll(); err != nil {
		t.Fatal(err)
	}
	mains, _ := database.GetNodeByName("main")
	closes, _ := database.GetNodeByName("close")
	if len(mains) == 0 || len(closes) == 0 {
		t.Fatal("missing nodes")
	}
	cals, _ := database.GetCallees(mains[0].ID)
	found := false
	for _, c := range cals {
		if c.Name == "close" {
			found = true
		}
	}
	if !found {
		t.Fatalf("main should call close via graph, callees=%+v", cals)
	}
}

func TestE2EProjectEmitStillResolves(t *testing.T) {
	// A real project function named emit must not be silenced by noise rules.
	dir, database, cleanup := setupIndexDir(t)
	defer cleanup()
	write(t, dir, "a.ts", `
export function emit(msg: string) { return msg }
export function run() { return emit("x") }
`)
	write(t, dir, "b.ts", `
import { emit } from "./a"
export function other() { emit("y") }
`)
	o := NewOrchestrator(database, dir)
	o.SetForceReindex(true)
	t.Setenv("CODEGRAPH_INDEX_WORKERS", "1")
	if _, _, err := o.IndexAll(); err != nil {
		t.Fatal(err)
	}
	runs, _ := database.GetNodeByName("run")
	emits, _ := database.GetNodeByName("emit")
	if len(runs) == 0 || len(emits) == 0 {
		t.Fatal("missing run/emit")
	}
	// same-file run → emit
	cals, _ := database.GetCallees(runs[0].ID)
	ok := false
	for _, c := range cals {
		if c.Name == "emit" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("run should call emit, got %+v", cals)
	}
}

func TestE2ESFCTemplatePrecision(t *testing.T) {
	src := `<template>
  <div class="wrap">
    <UserCard />
    <user-profile />
    <keep-alive><ChildPane/></keep-alive>
    <button @click="saveForm()">ok</button>
  </div>
</template>
<script lang="ts">
export function saveForm() {}
</script>
`
	res := NewExtractor("vue").Extract(src, "/src/Page.vue")
	names := map[string]string{}
	for _, r := range res.Refs {
		names[r.ReferenceName] = r.ReferenceKind
	}
	if names["UserCard"] != "references" {
		t.Fatalf("UserCard ref: %v", names)
	}
	if names["UserProfile"] != "references" {
		t.Fatalf("kebab user-profile → UserProfile missing: %v", names)
	}
	if names["ChildPane"] != "references" {
		t.Fatalf("ChildPane missing: %v", names)
	}
	if _, bad := names["KeepAlive"]; bad {
		t.Fatal("KeepAlive must not be a ref")
	}
	if _, bad := names["Div"]; bad {
		t.Fatal("Div must not be a ref")
	}
	if _, bad := names["Button"]; bad {
		t.Fatal("Button must not be a ref")
	}
	if names["saveForm"] != "calls" {
		t.Fatalf("saveForm @click call missing: %v", names)
	}
}

func TestE2EParallelMatchesSerial(t *testing.T) {
	mk := func(workers string) (nodes []string, edges []string) {
		dir, database, cleanup := setupIndexDir(t)
		defer cleanup()
		write(t, dir, "a.go", `package app
func A() { B() }
func B() {}
`)
		write(t, dir, "b.ts", `
export function foo() { bar() }
export function bar() {}
`)
		write(t, dir, "C.vue", `<template><Widget/></template>
<script>export function mount(){}</script>
`)
		write(t, dir, "w.ts", `export function Widget() { return null }
`)
		o := NewOrchestrator(database, dir)
		o.SetForceReindex(true)
		t.Setenv("CODEGRAPH_INDEX_WORKERS", workers)
		if _, _, err := o.IndexAll(); err != nil {
			t.Fatal(err)
		}
		// Snapshot: sorted "kind:name:qn" and "src>tgt:kind"
		files, _ := database.ListFiles()
		for _, f := range files {
			ns, _ := database.GetNodesByFile(f)
			for _, n := range ns {
				if n.Kind == db.KindFile {
					continue
				}
				nodes = append(nodes, fmt.Sprintf("%s|%s|%s|%v", n.Kind, n.Name, n.QualifiedName, n.IsExported))
				outs, _ := database.GetOutgoingEdges(n.ID, nil)
				for _, e := range outs {
					tgt, _ := database.GetNodeByID(e.TargetID)
					tname := "?"
					if tgt != nil {
						tname = tgt.Name
					}
					edges = append(edges, fmt.Sprintf("%s>%s:%s", n.Name, tname, e.Kind))
				}
			}
		}
		sort.Strings(nodes)
		sort.Strings(edges)
		return nodes, edges
	}

	n1, e1 := mk("1")
	n4, e4 := mk("4")
	if strings.Join(n1, "\n") != strings.Join(n4, "\n") {
		t.Fatalf("node snapshot mismatch serial vs parallel\nserial:\n%s\nparallel:\n%s", strings.Join(n1, "\n"), strings.Join(n4, "\n"))
	}
	if strings.Join(e1, "\n") != strings.Join(e4, "\n") {
		t.Fatalf("edge snapshot mismatch serial vs parallel\nserial:\n%s\nparallel:\n%s", strings.Join(e1, "\n"), strings.Join(e4, "\n"))
	}
}

func TestScrubNoisyButKeepRealSymbols(t *testing.T) {
	dir, database, cleanup := setupIndexDir(t)
	defer cleanup()
	// Only noise call, no emit symbol
	write(t, dir, "x.js", `
export function run() {
  console.log("hi")
  setState()
}
`)
	o := NewOrchestrator(database, dir)
	o.SetForceReindex(true)
	t.Setenv("CODEGRAPH_INDEX_WORKERS", "1")
	if _, _, err := o.IndexAll(); err != nil {
		t.Fatal(err)
	}
	pending, _ := database.CountUnresolvedRefs("pending")
	failed, _ := database.CountUnresolvedRefs("failed")
	// After scrub, setState/log must not linger
	allP, _ := database.ListUnresolvedRefs("", "pending")
	allF, _ := database.ListUnresolvedRefs("", "failed")
	for _, r := range append(allP, allF...) {
		if IsNoisyRefName(r.ReferenceName) {
			t.Fatalf("noisy ref still present: %+v (p=%d f=%d)", r, pending, failed)
		}
	}
}

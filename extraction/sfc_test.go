package extraction

import (
	"strings"
	"testing"
)

func TestSFCVueComponentAndScript(t *testing.T) {
	src := `<template>
  <div>
    <UserCard />
    <keep-alive><ChildPane /></keep-alive>
  </div>
</template>
<script lang="ts">
export function loadUser(id: number): string {
  console.log(id)
  return String(id)
}
function hidden() {}
</script>
`
	res := NewExtractor("vue").Extract(src, "/src/App.vue")

	comp := findNode(res.Nodes, "App")
	if comp == nil || comp.Kind != "component" || !comp.IsExported {
		t.Fatalf("file component: %+v", comp)
	}

	load := findNode(res.Nodes, "loadUser")
	if load == nil {
		t.Fatal("missing loadUser from script")
	}
	if load.Line < 6 {
		t.Errorf("loadUser line %d should be offset into the file", load.Line)
	}
	if !load.IsExported {
		t.Error("export function loadUser should be exported")
	}
	if !strings.Contains(load.Signature, "(id: number)") {
		t.Errorf("loadUser.Signature = %q", load.Signature)
	}

	// Template component refs
	hasUserCard, hasChild, hasKeepAlive := false, false, false
	for _, r := range res.Refs {
		if r.ReferenceName == "UserCard" && r.ReferenceKind == "references" {
			hasUserCard = true
		}
		if r.ReferenceName == "ChildPane" {
			hasChild = true
		}
		if r.ReferenceName == "KeepAlive" {
			hasKeepAlive = true
		}
	}
	if !hasUserCard {
		t.Error("missing template ref UserCard")
	}
	if !hasChild {
		t.Error("missing kebab→Pascal ChildPane from child-pane? (ChildPane tag)")
	}
	if hasKeepAlive {
		t.Error("KeepAlive is a Vue builtin and must be skipped")
	}

	// console.log noise must not be parked as a ref
	for _, r := range res.Refs {
		if r.ReferenceName == "log" || r.ReferenceName == "console.log" {
			t.Fatalf("noisy ref leaked: %+v", r)
		}
	}
}

func TestSFCSvelteAndAstro(t *testing.T) {
	svelte := `<script>
export function greet(name) { return name }
</script>
<button on:click={greet('x')}>hi</button>
<Modal />
`
	sres := NewExtractor("svelte").Extract(svelte, "/Widget.svelte")
	if findNode(sres.Nodes, "Widget") == nil {
		t.Fatal("svelte component node missing")
	}
	if findNode(sres.Nodes, "greet") == nil {
		t.Fatal("svelte script fn missing")
	}
	hasModal := false
	for _, r := range sres.Refs {
		if r.ReferenceName == "Modal" {
			hasModal = true
		}
	}
	if !hasModal {
		t.Error("svelte template Modal ref missing")
	}

	astro := `---
export function setup(): void {}
---
<html><BodyPanel /></html>
<script>
function clientOnly() {}
</script>
`
	ares := NewExtractor("astro").Extract(astro, "/page.astro")
	if findNode(ares.Nodes, "page") == nil {
		t.Fatal("astro component missing")
	}
	if findNode(ares.Nodes, "setup") == nil {
		t.Fatal("astro frontmatter setup missing")
	}
	if findNode(ares.Nodes, "clientOnly") == nil {
		t.Fatal("astro script clientOnly missing")
	}
	hasPanel := false
	for _, r := range ares.Refs {
		if r.ReferenceName == "BodyPanel" {
			hasPanel = true
		}
	}
	if !hasPanel {
		t.Error("astro template BodyPanel ref missing")
	}
}

func TestNoisyRefName(t *testing.T) {
	for _, n := range []string{"setState", "emit", "on", "cb", "log", "defineProps", "$state"} {
		if !IsNoisyRefName(n) {
			t.Errorf("%q should be noisy", n)
		}
	}
	if !IsNoisyRefName("console.log") {
		t.Error("console.log tail should be noisy")
	}
	// Real domain / constructor / Go-ish names must never be treated as noise.
	for _, n := range []string{"loadUser", "UserCard", "Hello", "paintCanvas", "add", "new", "close", "remove", "delete", "handler", "make", "append", "len"} {
		if IsNoisyRefName(n) {
			t.Errorf("%q must NOT be noisy", n)
		}
	}
}

// Same-file calls named add/new/close must survive promote so orchestrator can link.
func TestNoisyDoesNotDropSameFileCalls(t *testing.T) {
	js := "export function add(a,b){return a+b}\nexport function main(){return add(1,2)}\n"
	jres := NewTreeSitterExtractor("javascript").Extract(js, "a.js")
	hasAdd := false
	for _, r := range jres.Refs {
		if r.FromName == "main" && r.ReferenceName == "add" {
			hasAdd = true
		}
	}
	if !hasAdd {
		t.Fatal("same-file add() call must not be dropped at promote")
	}

	rust := "pub struct W{}\nimpl W { pub fn new() -> W { W{} } }\npub fn boot() { let _ = W::new(); }\n"
	rres := NewExtractor("rust").Extract(rust, "lib.rs")
	hasNew := false
	for _, r := range rres.Refs {
		if r.ReferenceName == "new" {
			hasNew = true
		}
	}
	if !hasNew {
		t.Fatal("Rust W::new() call must not be dropped at promote")
	}
}

func TestIndexWorkerCountEnv(t *testing.T) {
	t.Setenv("CODEGRAPH_INDEX_WORKERS", "1")
	if indexWorkerCount() != 1 {
		t.Fatalf("want 1, got %d", indexWorkerCount())
	}
	t.Setenv("CODEGRAPH_INDEX_WORKERS", "99")
	if indexWorkerCount() != 16 {
		t.Fatalf("cap 16, got %d", indexWorkerCount())
	}
	t.Setenv("CODEGRAPH_INDEX_WORKERS", "0")
	if indexWorkerCount() != 1 {
		t.Fatalf("0 → 1, got %d", indexWorkerCount())
	}
}

func TestKebabToPascal(t *testing.T) {
	if g := kebabToPascal("user-card"); g != "UserCard" {
		t.Fatalf("got %q", g)
	}
	if g := kebabToPascal("keep-alive"); g != "KeepAlive" {
		t.Fatalf("got %q", g)
	}
}

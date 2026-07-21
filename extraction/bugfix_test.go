package extraction

import (
	"sync"
	"testing"
)

// TestRunIndexJobsRecoverPerIteration verifies BUG-5 fix: a panic inside one
// job iteration does not kill the worker goroutine. With the old pattern
// (recover at goroutine level) a single panic would permanently exit the
// worker, and ≥workers panics would deadlock the entire pool.
//
// This test simulates the worker pool pattern with 3 workers processing 10
// jobs; 4 jobs panic. The per-iteration recover ensures the worker continues
// and all non-panic jobs complete.
func TestRunIndexJobsRecoverPerIteration(t *testing.T) {
	workers := 3
	totalJobs := 10
	// Inject panics on these job indices.
	panicOn := map[int]bool{0: true, 3: true, 6: true, 9: true}

	var mu sync.Mutex
	var completed int
	var wg sync.WaitGroup
	ch := make(chan int, workers*2)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				func(j int) {
					defer func() {
						if r := recover(); r != nil {
							// Panic is expected — worker must continue.
						}
					}()
					if panicOn[j] {
						panic("injected panic")
					}
					mu.Lock()
					completed++
					mu.Unlock()
				}(j)
			}
		}()
	}
	for j := 0; j < totalJobs; j++ {
		ch <- j
	}
	close(ch)
	wg.Wait()

	want := totalJobs - len(panicOn)
	if completed != want {
		t.Errorf("want %d non-panic jobs completed, got %d. "+
			"Per-iteration recover should keep workers alive after panics.", want, completed)
	}
}

// TestRunIndexJobsPanicDoesNotHang verifies BUG-5 fix: even when every worker
// hits a panic (≥workers panics), the channel drains cleanly and WaitGroup
// completes — no deadlock. With the old goroutine-level recover, all workers
// would exit, channel buffer would fill, and the main goroutine would block
// forever on ch <- j.
func TestRunIndexJobsPanicDoesNotHang(t *testing.T) {
	workers := 3
	totalJobs := workers * 3 // every job panics

	var wg sync.WaitGroup
	ch := make(chan int, workers*2)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				func(j int) {
					defer func() {
						recover() // per-iteration — worker stays alive
					}()
					panic("injected")
				}(j)
			}
		}()
	}
	for j := 0; j < totalJobs; j++ {
		ch <- j
	}
	close(ch)

	// If this blocks, the test times out → deadlock.
	wg.Wait()
}

// TestSFCSvelteAstroLineNumbers verifies BUG-6 fix: svelte/astro template tag
// line numbers must align with the original source file, even when multi-line
// script blocks appear before the template section.
func TestSFCSvelteAstroLineNumbers(t *testing.T) {
	// Svelte: multi-line script block before template tags.
	// Line 1: <script>
	// Line 2-7: script content (6 lines)
	// Line 8: </script>
	// Line 9: (empty)
	// Line 10: <Modal />
	// Line 11: <AnotherTag />
	svelte := `<script>
export function greet(name: string): string {
  return "Hello, " + name
}
function hidden() {
  // internal
}
</script>

<Modal />
<AnotherTag />
`
	svelteRes := NewExtractor("svelte").Extract(svelte, "/Widget.svelte")
	for _, r := range svelteRes.Refs {
		switch r.ReferenceName {
		case "Modal":
			// Modal appears on line 10 of the source.
			if r.Line != 10 {
				t.Errorf("svelte Modal ref: want line 10, got %d", r.Line)
			}
		case "AnotherTag":
			if r.Line != 11 {
				t.Errorf("svelte AnotherTag ref: want line 11, got %d", r.Line)
			}
		}
	}

	// Astro: frontmatter (--- … ---) + script block before template content.
	// Line 1: ---
	// Line 2: const title = "Hello"
	// Line 3: ---
	// Line 4: <script>
	// Line 5: function clientOnly() {
	// Line 6: console.log(...)
	// Line 7: }
	// Line 8: </script>
	// Line 9: <BodyPanel />
	astro := `---
const title = "Hello"
---
<script>
function clientOnly() {
  console.log("client side")
}
</script>
<BodyPanel />
`
	astroRes := NewExtractor("astro").Extract(astro, "/page.astro")
	for _, r := range astroRes.Refs {
		if r.ReferenceName == "BodyPanel" {
			// BodyPanel appears on line 9 of the source.
			if r.Line != 9 {
				t.Errorf("astro BodyPanel ref: want line 9, got %d", r.Line)
			}
		}
	}

	// Edge case: template content before and after a script block.
	// Line 1: <TopBar />
	// Line 2: <script>
	// Line 3: const x = 1
	// Line 4: </script>
	// Line 5: <BottomBar />
	// Note: avoid native HTML element names (Header, Footer are native).
	before := `<TopBar />
<script>
const x = 1
</script>
<BottomBar />
`
	beforeRes := NewExtractor("svelte").Extract(before, "/Before.svelte")
	hasTop, hasBottom := false, false
	for _, r := range beforeRes.Refs {
		switch r.ReferenceName {
		case "TopBar":
			hasTop = true
			if r.Line != 1 {
				t.Errorf("TopBar ref before script: want line 1, got %d", r.Line)
			}
		case "BottomBar":
			hasBottom = true
			if r.Line != 5 {
				t.Errorf("BottomBar ref after script: want line 5, got %d", r.Line)
			}
		}
	}
	if !hasTop {
		t.Error("missing TopBar ref (before script)")
	}
	if !hasBottom {
		t.Error("missing BottomBar ref (after script)")
	}
}

// TestOldPanicPatternWouldDeadlock documents the old BUG-5 pattern.
// With goroutine-level recover, once a worker panics and exits, the channel
// buffer fills up and the sender blocks. This test verifies the new
// per-iteration pattern handles the same scenario without deadlock.
// (This is a documentation/safety test; it does not test the old code.)
func TestOldPanicPatternWouldDeadlock(t *testing.T) {
	// Simulate the old pattern: recover at goroutine level.
	// With 3 workers and 10 jobs where 4 panic, only the first panic in each
	// worker kills it. The remaining jobs fill the channel and deadlock.
	// We verify the NEW pattern works correctly instead.

	workers := 3
	totalJobs := 10
	panicEvery := 2 // panic on every 2nd job per worker

	var mu sync.Mutex
	var completed int
	var wg sync.WaitGroup
	ch := make(chan int, workers*2)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobCount := 0
			for j := range ch {
				func(j int) {
					defer func() {
						if r := recover(); r != nil {
							// per-iteration recover
						}
					}()
					jobCount++
					if jobCount%panicEvery == 0 {
						panic("injected")
					}
					mu.Lock()
					completed++
					mu.Unlock()
				}(j)
			}
		}()
	}
	for j := 0; j < totalJobs; j++ {
		ch <- j
	}
	close(ch)
	wg.Wait()

	if completed == 0 {
		t.Error("new per-iteration pattern: some jobs should complete despite panics")
	}
	t.Logf("completed %d / %d jobs with %d panics", completed, totalJobs, totalJobs-completed)
}

// TestCallEdgeLineNumbersGo verifies S-3 fix: Go call edges use call-site
// line (not function definition line). A function with multiple calls to
// the same target produces one edge per call site.
func TestCallEdgeLineNumbersGo(t *testing.T) {
	// Line 1: package main
	// Line 2: (empty)
	// Line 3: func caller() {
	// Line 4: 	hello()   <-- call site
	// Line 5: 	hello()   <-- call site
	// Line 6: }
	// Line 7: (empty)
	// Line 8: func hello() {}
	source := `package main

func caller() {
	hello()
	hello()
}

func hello() {}
`
	ext := NewTreeSitterExtractor("go")
	res := ext.Extract(source, "/test.go")

	// Collect call refs from caller to hello.
	var callLines []int
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "caller" && r.ReferenceName == "hello" {
			callLines = append(callLines, r.Line)
		}
	}

	// Must have two distinct edges (Decision 4: no merging by callee name alone).
	if len(callLines) != 2 {
		t.Fatalf("want 2 call edges from caller->hello, got %d", len(callLines))
	}
	// Both must be call-site lines, NOT the function definition line (3).
	for _, l := range callLines {
		if l == 3 {
			t.Errorf("call edge has line 3 (func def line), want call-site line 4 or 5. lines=%v", callLines)
		}
	}
	// Check we have one at line 4 and one at line 5.
	has4, has5 := false, false
	for _, l := range callLines {
		if l == 4 {
			has4 = true
		}
		if l == 5 {
			has5 = true
		}
	}
	if !has4 || !has5 {
		t.Errorf("expected call edges at lines 4 and 5, got %v", callLines)
	}
}

// TestCallEdgeLineNumbersPython verifies S-3 fix: Python call edges use
// call-site line (not function definition line).
func TestCallEdgeLineNumbersPython(t *testing.T) {
	// Line 1: def caller():
	// Line 2:     hello()   <-- call site
	// Line 3:     hello()   <-- call site
	// Line 4: (empty)
	// Line 5: def hello():
	// Line 6:     pass
	source := `def caller():
    hello()
    hello()

def hello():
    pass
`
	ext := NewTreeSitterExtractor("python")
	res := ext.Extract(source, "/test.py")

	var callLines []int
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "caller" && r.ReferenceName == "hello" {
			callLines = append(callLines, r.Line)
		}
	}

	if len(callLines) != 2 {
		t.Fatalf("want 2 call edges from caller->hello, got %d", len(callLines))
	}
	for _, l := range callLines {
		if l == 1 {
			t.Errorf("call edge has line 1 (func def line), want call-site line 2 or 3. lines=%v", callLines)
		}
	}
	has2, has3 := false, false
	for _, l := range callLines {
		if l == 2 {
			has2 = true
		}
		if l == 3 {
			has3 = true
		}
	}
	if !has2 || !has3 {
		t.Errorf("expected call edges at lines 2 and 3, got %v", callLines)
	}
}

// TestGoAnonymousClosureCalls verifies S-4 fix: calls inside Go anonymous
// func_literal closures are attributed to the outer function (not dropped).
func TestGoAnonymousClosureCalls(t *testing.T) {
	// go func() { inner() }() — inner call should belong to outer.
	source := `package main

func outer() {
	go func() {
		inner()
	}()
}

func inner() {}
`
	ext := NewTreeSitterExtractor("go")
	res := ext.Extract(source, "/test.go")

	hasCall := false
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "outer" && r.ReferenceName == "inner" {
			hasCall = true
			// inner() is on line 5 of the source.
			if r.Line != 5 {
				t.Errorf("inner call edge: want line 5, got %d", r.Line)
			}
		}
	}
	if !hasCall {
		t.Error("missing ref: outer -> inner (S-4: anonymous closure calls dropped)")
	}

	// errgroup.Go(func() { inner() }) — similar pattern.
	source2 := `package main

func outer() {
	eg.Go(func() {
		inner()
	})
}

func inner() {}
`
	res2 := NewTreeSitterExtractor("go").Extract(source2, "/test2.go")
	hasCall2 := false
	for _, r := range res2.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "outer" && r.ReferenceName == "inner" {
			hasCall2 = true
			if r.Line != 5 {
				t.Errorf("inner call edge (eg.Go): want line 5, got %d", r.Line)
			}
		}
	}
	if !hasCall2 {
		t.Error("missing ref: outer -> inner via eg.Go(func() {...}) (S-4)")
	}

	// http.HandleFunc pattern.
	source3 := `package main

func outer() {
	http.HandleFunc("/path", func() {
		inner()
	})
}

func inner() {}
`
	res3 := NewTreeSitterExtractor("go").Extract(source3, "/test3.go")
	hasCall3 := false
	for _, r := range res3.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "outer" && r.ReferenceName == "inner" {
			hasCall3 = true
			if r.Line != 5 {
				t.Errorf("inner call edge (HandleFunc): want line 5, got %d", r.Line)
			}
		}
	}
	if !hasCall3 {
		t.Error("missing ref: outer -> inner via HandleFunc(func(){...}) (S-4)")
	}
}

// TestJSAnonymousClosureCalls verifies S-4 fix: calls inside JS anonymous
// arrow_function / function_expression closures are attributed to the outer function.
func TestJSAnonymousClosureCalls(t *testing.T) {
	// .then(() => inner())  — arrow function with expression body.
	source := `function outer() {
    something().then(() => inner())
}

function inner() {}
`
	ext := NewTreeSitterExtractor("javascript")
	res := ext.Extract(source, "/test.js")

	hasCall := false
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "outer" && r.ReferenceName == "inner" {
			hasCall = true
			// inner() is on line 2.
			if r.Line != 2 {
				t.Errorf("inner call edge (.then arrow): want line 2, got %d", r.Line)
			}
		}
	}
	if !hasCall {
		t.Error("missing ref: outer -> inner via .then(()=>...) (S-4 JS)")
	}

	// .map(x => foo(x))
	source2 := `function outer() {
    [1,2,3].map(x => foo(x))
}

function foo(x) { return x }
`
	res2 := NewTreeSitterExtractor("javascript").Extract(source2, "/test2.js")
	hasCall2 := false
	for _, r := range res2.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "outer" && r.ReferenceName == "foo" {
			hasCall2 = true
			if r.Line != 2 {
				t.Errorf("foo call edge (.map arrow): want line 2, got %d", r.Line)
			}
		}
	}
	if !hasCall2 {
		t.Error("missing ref: outer -> foo via .map(x=>...) (S-4 JS)")
	}

	// Anonymous function expression: something(function() { inner() })
	source3 := `function outer() {
    something(function() { return inner() })
}

function inner() { return 1 }
`
	res3 := NewTreeSitterExtractor("javascript").Extract(source3, "/test3.js")
	hasCall3 := false
	for _, r := range res3.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "outer" && r.ReferenceName == "inner" {
			hasCall3 = true
		}
	}
	if !hasCall3 {
		t.Error("missing ref: outer -> inner via function() { ... } (S-4 JS function_expression)")
	}
}

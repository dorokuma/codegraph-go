package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/extraction"
	"github.com/dorokuma/codegraph-go/internal/resolution"
	codesync "github.com/dorokuma/codegraph-go/sync"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func setupTempDB(t *testing.T) (*db.DB, func()) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return database, func() { database.Close() }
}

func writeGoFile(dir, name, content string) string {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
	return path
}

// ---------------------------------------------------------------------------
// BUG-1: Concurrent DB read/write — no SQLITE_BUSY, no panic, correct counts
// ---------------------------------------------------------------------------

func TestConcurrentDBReadWrite(t *testing.T) {
	database, cleanup := setupTempDB(t)
	defer cleanup()

	const (
		numGoroutines = 8
		opsPerRoutine = 100
	)

	var wg sync.WaitGroup
	var panicCount int32

	startBarrier := make(chan struct{})

	for g := 0; g < numGoroutines; g++ {
		g := g // capture
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					atomic.AddInt32(&panicCount, 1)
				}
			}()

			// Each goroutine uses its own file prefix to avoid
			// ClearFile races with other goroutines' UpsertEdge.
			prefix := fmt.Sprintf("/g%d", g)
			fileA := prefix + "/a.go"
			fileB := prefix + "/b.go"

			// Pre-populate one node in each file so edges have valid targets.
			srcName := fmt.Sprintf("Src%d", g)
			dstName := fmt.Sprintf("Dst%d", g)

			<-startBarrier

			for i := 0; i < opsPerRoutine; i++ {
				// UpsertNode in fileA
				srcID, err := database.UpsertNode(&db.Node{
					Kind:     db.KindFunction,
					Name:     srcName,
					File:     fileA,
					Line:     i + 1,
					Body:     fmt.Sprintf("func %s() { %s() }", srcName, dstName),
					Language: "go",
				})
				if err != nil {
					t.Errorf("upsert src %d/%d: %v", g, i, err)
					return
				}

				// UpsertNode in fileB
				dstID, err := database.UpsertNode(&db.Node{
					Kind:     db.KindFunction,
					Name:     dstName,
					File:     fileB,
					Line:     i + 1,
					Body:     fmt.Sprintf("func %s() {}", dstName),
					Language: "go",
				})
				if err != nil {
					t.Errorf("upsert dst %d/%d: %v", g, i, err)
					return
				}

				// UpsertEdge
				if _, err := database.UpsertEdge(&db.Edge{
					SourceID:   srcID,
					TargetID:   dstID,
					Kind:       db.EdgeCalls,
					File:       fileA,
					Line:       i + 1,
					Provenance: "exact",
				}); err != nil {
					t.Errorf("upsert edge %d/%d: %v", g, i, err)
					return
				}

				// FullTextSearch (read lock)
				if _, err := database.FullTextSearch(srcName, 10); err != nil {
					t.Errorf("fts %d/%d: %v", g, i, err)
					return
				}

				// ClearFile on a third file (not interfering with above)
				cleanFile := fmt.Sprintf("%s/clean_%d.go", prefix, i)
				// Write a dummy node first
				database.UpsertNode(&db.Node{
					Kind: db.KindFunction, Name: "dummy", File: cleanFile,
					Line: 1, Body: "dummy", Language: "go",
				})
				if err := database.ClearFile(cleanFile); err != nil {
					t.Errorf("clear file %d/%d: %v", g, i, err)
					return
				}
			}
		}()
	}

	close(startBarrier)
	wg.Wait()

	if atomic.LoadInt32(&panicCount) > 0 {
		t.Fatalf("panicCount=%d, expected 0", panicCount)
	}

	// Verify PRAGMA foreign_keys is ON (no orphan edges) and busy_timeout works.
	// - No SQLITE_BUSY during the test = busy_timeout works.
	// - No orphan edges = foreign_keys ON with CASCADE works.
	// Each goroutine did opsPerRoutine iterations, each creating 2 nodes (src+dst)
	// at different lines and 1 edge. Clean files were cleared.
	// So we expect numGoroutines*opsPerRoutine*2 nodes and numGoroutines*opsPerRoutine edges.
	stats, err := database.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	expectedNodes := numGoroutines * opsPerRoutine * 2
	expectedEdges := numGoroutines * opsPerRoutine
	if stats.NodeCount != expectedNodes {
		t.Errorf("node count: got %d, want %d", stats.NodeCount, expectedNodes)
	}
	if stats.EdgeCount != expectedEdges {
		t.Errorf("edge count: got %d, want %d (possible orphan edges or FK failure)",
			stats.EdgeCount, expectedEdges)
	}
}

// ---------------------------------------------------------------------------
// BUG-2: Watcher Stop does not hang (returns <2s, DB not closed mid-write)
// ---------------------------------------------------------------------------

func TestStressWatcherStopNoHang(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create several go files so the watcher has something to index.
	for i := 0; i < 20; i++ {
		writeGoFile(dir, fmt.Sprintf("file%d.go", i),
			fmt.Sprintf("package p\nfunc F%d() {}\n", i))
	}
	// Create subdir with more files.
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0o755)
	for i := 0; i < 10; i++ {
		writeGoFile(sub, fmt.Sprintf("sub%d.go", i),
			fmt.Sprintf("package sub\nfunc S%d() {}\n", i))
	}

	orch := extraction.NewOrchestrator(database, dir)
	w, err := codesync.NewWatcher(orch, dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Start(); err != nil {
		t.Fatal(err)
	}

	// Let the watcher run for a bit to process some pending events.
	time.Sleep(500 * time.Millisecond)

	// Stop must return within 2 seconds.
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK — returned within timeout.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2 seconds")
	}

	// After Stop, DB should still be usable (not closed).
	_, err = database.GetStats()
	if err != nil {
		t.Fatalf("DB not usable after watcher Stop: %v", err)
	}
}

// ---------------------------------------------------------------------------
// M-22: IndexAllWithProgress concurrent onProgress (no data race)
// ---------------------------------------------------------------------------

func TestIndexAllProgressConcurrent(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create enough files to trigger parallel workers (indexWorkerCount() >= 2).
	for i := 0; i < 50; i++ {
		fname := fmt.Sprintf("pkg%d.go", i)
		body := fmt.Sprintf("package p\nfunc Func%d() int { return %d }\n", i, i)
		writeGoFile(dir, fname, body)
	}

	orch := extraction.NewOrchestrator(database, dir)

	var mu sync.Mutex
	var progressCalls int
	var lastPhase string
	var lastTotal int

	onProgress := func(phase string, current, total int) {
		mu.Lock()
		progressCalls++
		lastPhase = phase
		lastTotal = total
		mu.Unlock()
	}

	fileCount, _, err := orch.IndexAllWithProgress(onProgress)
	if err != nil {
		t.Fatalf("IndexAllWithProgress: %v", err)
	}

	if fileCount != 50 {
		t.Errorf("indexed file count: got %d, want 50", fileCount)
	}
	if progressCalls == 0 {
		t.Error("onProgress was never called")
	}
	if lastPhase != "indexing" {
		t.Errorf("last phase: got %q, want indexing", lastPhase)
	}
	if lastTotal == 0 {
		t.Error("lastTotal should be > 0")
	}

	// progressCalls should not exceed fileCount (called at most once per file).
	if progressCalls > fileCount+1 {
		t.Errorf("progressCalls=%d > fileCount+1=%d", progressCalls, fileCount+1)
	}
}

// ---------------------------------------------------------------------------
// M-4: goroutine leak — IndexAll + SynthesizeAll delta < 10
// ---------------------------------------------------------------------------

func TestGoroutineLeakAfterIndexAll(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create several go files.
	for i := 0; i < 30; i++ {
		writeGoFile(dir, fmt.Sprintf("f%d.go", i),
			fmt.Sprintf("package p\nfunc F%d() int { return %d }\n", i, i))
	}

	// Let runtime settle.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	before := runtime.NumGoroutine()

	orch := extraction.NewOrchestrator(database, dir)
	_, _, err = orch.IndexAll()
	if err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	// SynthesizeAll is called inside IndexAll via runSynthesis, but let's
	// also call it directly to cover the full path.
	_, err = resolution.SynthesizeAll(database, dir)
	if err != nil {
		t.Fatalf("SynthesizeAll: %v", err)
	}

	// Let goroutines settle.
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()

	delta := after - before
	if delta < 0 {
		delta = -delta
	}
	if delta >= 10 {
		t.Errorf("goroutine delta too high: before=%d after=%d delta=%d (want <10)",
			before, after, delta)
	} else {
		t.Logf("goroutines: before=%d after=%d delta=%d", before, after, delta)
	}
}

// ---------------------------------------------------------------------------
// S-7: FileContent LRU bound — run large dir synthesis, assert completion
// ---------------------------------------------------------------------------

func TestStressFileContentLRU(t *testing.T) {
	// We cannot directly observe the fileContent LRU cache inside
	// resolution.synthCtx (unexported), but we can verify that a large
	// synthesis completes without error and without memory explosion.
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create 300 small go files to exceed the fileContent LRU limit of 256.
	for i := 0; i < 300; i++ {
		writeGoFile(dir, fmt.Sprintf("g%d.go", i),
			fmt.Sprintf("package p\nfunc G%d() string { return \"hello\" }\n", i))
	}

	orch := extraction.NewOrchestrator(database, dir)
	fileCount, _, err := orch.IndexAll()
	if err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	if fileCount != 300 {
		t.Errorf("indexed %d files, want 300", fileCount)
	}

	// SynthesizeAll with the large file set.
	// This internally uses fileContent LRU capped at 256.
	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	_, err = resolution.SynthesizeAll(database, dir)
	if err != nil {
		t.Fatalf("SynthesizeAll: %v", err)
	}

	var memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memAfter)

	// Check heap didn't explode (allow reasonable growth, say < 50MB delta).
	heapDelta := int64(memAfter.HeapInuse) - int64(memBefore.HeapInuse)
	if heapDelta > 50*1024*1024 {
		t.Errorf("HeapInuse grew by %d bytes (> 50MB), possible memory leak", heapDelta)
	} else {
		t.Logf("HeapInuse delta: %d bytes", heapDelta)
	}
}

// ---------------------------------------------------------------------------
// Long-run memory: 10x IndexFile + SynthesizeAll, HeapInuse not monotonic grow
// ---------------------------------------------------------------------------

func TestLongRunMemoryStable(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	srcPath := writeGoFile(dir, "stable.go", `package p

func Stable() int {
	return 42
}

func Helper() string {
	return "ok"
}
`)

	orch := extraction.NewOrchestrator(database, dir)
	const cycles = 10

	var heapSamples []uint64

	for i := 0; i < cycles; i++ {
		// IndexFile the same file repeatedly.
		if _, err := orch.IndexFile(srcPath); err != nil {
			t.Fatalf("IndexFile cycle %d: %v", i, err)
		}
		if _, err := resolution.SynthesizeAll(database, dir); err != nil {
			t.Fatalf("SynthesizeAll cycle %d: %v", i, err)
		}

		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		heapSamples = append(heapSamples, ms.HeapInuse)
	}

	// Check that HeapInuse does not monotonically increase.
	// Allow some noise but if every sample is >= previous, it's a leak.
	increases := 0
	for i := 1; i < len(heapSamples); i++ {
		if heapSamples[i] > heapSamples[i-1] {
			increases++
		}
	}
	if increases == len(heapSamples)-1 {
		t.Errorf("HeapInuse monotonically increased across all %d cycles: %v", cycles, heapSamples)
	} else {
		t.Logf("HeapInuse samples: %v (increases: %d/%d)", heapSamples, increases, cycles-1)
	}
}

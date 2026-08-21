package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// broadHome makes base count as a broad (home-mode) workdir for detectProject
// by pointing $HOME at it (IsBroadWorkdir checks os.UserHomeDir).
func broadHome(t *testing.T, base string) {
	t.Helper()
	t.Setenv("HOME", base)
}

// writeProjectDir creates a directory that looks like an indexed project
// (has a project marker).
func writeProjectDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectProjectBasic(t *testing.T) {
	base := t.TempDir()
	broadHome(t, base)
	proj := filepath.Join(base, "myrepo")
	writeProjectDir(t, proj)
	// A plain directory without a project marker must never be detected.
	if err := os.MkdirAll(filepath.Join(base, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Server{Workdir: base, Workdirs: []string{base}}

	// Exact base-name match.
	if got := s.detectProject("myrepo"); got != proj {
		t.Fatalf("detectProject(myrepo) = %q, want %q", got, proj)
	}
	// Sentence mention must not switch projects (home-mode footgun).
	if got := s.detectProject("please look at myrepo now"); got != "" {
		t.Fatalf("detectProject sentence = %q, want empty", got)
	}
	// Path prefix still selects the project.
	if got := s.detectProject("myrepo/internal"); got != proj {
		t.Fatalf("detectProject path prefix = %q, want %q", got, proj)
	}
	// Case-insensitive exact match.
	if got := s.detectProject("MyRepo"); got != proj {
		t.Fatalf("detectProject(MyRepo) = %q, want %q", got, proj)
	}
	// Non-project directory: no detection.
	if got := s.detectProject("plain"); got != "" {
		t.Fatalf("detectProject(plain) = %q, want empty", got)
	}
	if got := stripDetectedProjectPrefix("myrepo", proj); got != "" {
		t.Fatalf("strip myrepo = %q, want empty", got)
	}
	if got := stripDetectedProjectPrefix("myrepo/internal", proj); got != "internal" {
		t.Fatalf("strip myrepo/internal = %q, want internal", got)
	}
	if got := stripDetectedProjectPrefix("myrepo/", proj); got != "" {
		t.Fatalf("strip myrepo/ = %q, want empty", got)
	}

	// Cache semantics: after the first scan (DetectDone), removing the marker
	// must not change results until resetDetect.
	if err := os.Remove(filepath.Join(proj, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if got := s.detectProject("myrepo"); got != proj {
		t.Fatalf("cached detect = %q, want %q", got, proj)
	}
	// resetDetect clears the cache; the next call rescans and sees the
	// marker is gone.
	s.resetDetect()
	if got := s.detectProject("myrepo"); got != "" {
		t.Fatalf("detect after reset = %q, want empty (marker removed)", got)
	}
}

// TestDetectProjectConcurrentReset is a race guard: detectProject used to
// iterate the live DetectDirs slice after releasing DetectMu while
// resetDetect could clear it concurrently (data race). Run both concurrently
// under -race; the final sequential detect also proves the cache still works.
func TestDetectProjectConcurrentReset(t *testing.T) {
	base := t.TempDir()
	broadHome(t, base)
	for i := 0; i < 3; i++ {
		writeProjectDir(t, filepath.Join(base, fmt.Sprintf("proj-%d", i)))
	}

	s := &Server{Workdir: base, Workdirs: []string{base}}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.detectProject("proj-1", "proj-2")
		}()
		go func() {
			defer wg.Done()
			s.resetDetect()
		}()
	}
	wg.Wait()

	// After all the concurrent churn the cache still produces correct results.
	if got := s.detectProject("proj-1"); got != filepath.Join(base, "proj-1") {
		t.Fatalf("detectProject after concurrent reset = %q", got)
	}
}

func TestResolveProjectPendingCloseReuse(t *testing.T) {
	base := t.TempDir()
	projA := filepath.Join(base, "projA")
	projB := filepath.Join(base, "projB")
	writeProjectDir(t, projA)
	writeProjectDir(t, projB)

	dbA, err := db.Open(projA)
	if err != nil {
		t.Fatal(err)
	}
	dbA.Close()

	dbB, err := db.Open(projB)
	if err != nil {
		t.Fatal(err)
	}
	dbB.Close()

	baseDB, err := db.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer baseDB.Close()

	s := &Server{
		Workdir:       base,
		Workdirs:      []string{base},
		Database:      baseDB,
		ProjectMaxLRU: 1,
	}
	defer s.closeProjectCache()

	// 1. Resolve projA (cache: projA, refs=1)
	rootA, d1, err := s.resolveProject(projA)
	if err != nil {
		t.Fatalf("resolve projA: %v", err)
	}
	if rootA != projA {
		t.Fatalf("rootA = %q, want %q", rootA, projA)
	}
	if s.ProjectCache[projA] == nil || atomic.LoadInt32(&s.ProjectCache[projA].refs) != 1 {
		t.Fatalf("expected projA in cache with refs=1")
	}

	// 2. Resolve projB while projA is still in-flight (MaxLRU=1 triggers eviction of projA to PendingClose)
	rootB, _, err := s.resolveProject(projB)
	if err != nil {
		t.Fatalf("resolve projB: %v", err)
	}
	if rootB != projB {
		t.Fatalf("rootB = %q, want %q", rootB, projB)
	}
	if s.ProjectPendingClose[projA] == nil || atomic.LoadInt32(&s.ProjectPendingClose[projA].refs) != 1 {
		t.Fatalf("expected projA in pending-close with refs=1")
	}
	if s.ProjectCache[projB] == nil || atomic.LoadInt32(&s.ProjectCache[projB].refs) != 1 {
		t.Fatalf("expected projB in cache with refs=1")
	}

	// 3. Re-access projA while in PendingClose: must reuse existing handle and restore to cache without second db.Open
	rootA2, d3, err := s.resolveProject(projA)
	if err != nil {
		t.Fatalf("re-resolve projA from pending-close failed: %v", err)
	}
	if rootA2 != projA {
		t.Fatalf("rootA2 = %q, want %q", rootA2, projA)
	}
	if d3 != d1 {
		t.Fatalf("expected d3 == d1 (reused db handle), got %p != %p", d3, d1)
	}
	if s.ProjectPendingClose[projA] != nil {
		t.Fatalf("expected projA removed from pending-close")
	}
	if s.ProjectCache[projA] == nil || atomic.LoadInt32(&s.ProjectCache[projA].refs) != 2 {
		t.Fatalf("expected projA in cache with refs=2")
	}
	// projB was evicted and moved to PendingClose because refs=1
	if s.ProjectPendingClose[projB] == nil || atomic.LoadInt32(&s.ProjectPendingClose[projB].refs) != 1 {
		t.Fatalf("expected projB in pending-close with refs=1")
	}

	// 4. Release projA first caller
	s.releaseProject(projA)
	if atomic.LoadInt32(&s.ProjectCache[projA].refs) != 1 {
		t.Fatalf("expected projA refs=1 after first release")
	}

	// 5. Release projA second caller
	s.releaseProject(projA)
	if atomic.LoadInt32(&s.ProjectCache[projA].refs) != 0 {
		t.Fatalf("expected projA refs=0 after second release")
	}

	// 6. Release projB caller -> should close and remove from PendingClose
	s.releaseProject(projB)
	if s.ProjectPendingClose[projB] != nil {
		t.Fatalf("expected projB cleaned from pending-close after release")
	}
}

func TestResolveProjectOpenFailureRollback(t *testing.T) {
	base := t.TempDir()
	projA := filepath.Join(base, "projA")
	projB := filepath.Join(base, "projB")
	writeProjectDir(t, projA)
	writeProjectDir(t, projB)

	dbA, err := db.Open(projA)
	if err != nil {
		t.Fatal(err)
	}
	dbA.Close()

	// projB has .codegraph directory, but codegraph.lock is locked by another file descriptor
	// so db.Open(projB) will fail on flock.
	if err := os.MkdirAll(filepath.Join(projB, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(filepath.Join(projB, ".codegraph", "codegraph.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	baseDB, err := db.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer baseDB.Close()

	s := &Server{
		Workdir:       base,
		Workdirs:      []string{base},
		Database:      baseDB,
		ProjectMaxLRU: 1,
	}
	defer s.closeProjectCache()

	// Subcase 1: Rollback when evicted entry has refs > 0
	_, d1, err := s.resolveProject(projA)
	if err != nil {
		t.Fatalf("resolve projA: %v", err)
	}
	if s.ProjectCache[projA] == nil || atomic.LoadInt32(&s.ProjectCache[projA].refs) != 1 {
		t.Fatalf("expected projA in cache with refs=1")
	}

	// Attempt to resolve projB (which fails on db.Open)
	if _, _, err := s.resolveProject(projB); err == nil {
		t.Fatalf("expected resolve projB to fail due to lock")
	}

	// Verify rollback for refs > 0
	if s.ProjectCache[projA] == nil || s.ProjectCache[projA].db != d1 || atomic.LoadInt32(&s.ProjectCache[projA].refs) != 1 {
		t.Fatalf("expected projA restored in cache with refs=1")
	}
	if len(s.ProjectLRU) != 1 || s.ProjectLRU[0] != projA {
		t.Fatalf("expected LRU restored with projA, got %v", s.ProjectLRU)
	}
	if len(s.ProjectPendingClose) != 0 {
		t.Fatalf("expected pending-close to be empty, got %v", s.ProjectPendingClose)
	}

	// Release projA -> refs becomes 0
	s.releaseProject(projA)
	if atomic.LoadInt32(&s.ProjectCache[projA].refs) != 0 {
		t.Fatalf("expected projA refs=0")
	}

	// Subcase 2: Rollback when evicted entry has refs == 0
	// projA is in cache with refs=0. Attempt to resolve projB again.
	if _, _, err := s.resolveProject(projB); err == nil {
		t.Fatalf("expected resolve projB to fail")
	}

	// Verify rollback for refs == 0: DB must NOT have been closed!
	if s.ProjectCache[projA] == nil || s.ProjectCache[projA].db != d1 || atomic.LoadInt32(&s.ProjectCache[projA].refs) != 0 {
		t.Fatalf("expected projA restored in cache with refs=0")
	}
	if len(s.ProjectLRU) != 1 || s.ProjectLRU[0] != projA {
		t.Fatalf("expected LRU restored with projA, got %v", s.ProjectLRU)
	}
	if len(s.ProjectPendingClose) != 0 {
		t.Fatalf("expected pending-close to be empty, got %v", s.ProjectPendingClose)
	}

	// ProjA must still be usable (DB not closed)
	_, d2, err := s.resolveProject(projA)
	if err != nil {
		t.Fatalf("resolve projA after rollback failed: %v", err)
	}
	if d2 != d1 {
		t.Fatalf("expected d2 == d1 (unclosed db reused), got %p != %p", d2, d1)
	}
	s.releaseProject(projA)
}

func TestResolveProjectConcurrentAccessAndEviction(t *testing.T) {
	base := t.TempDir()
	numProjects := 5
	projs := make([]string, numProjects)
	for i := 0; i < numProjects; i++ {
		p := filepath.Join(base, fmt.Sprintf("proj-%d", i))
		writeProjectDir(t, p)
		d, err := db.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
		projs[i] = p
	}

	baseDB, err := db.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer baseDB.Close()

	s := &Server{
		Workdir:       base,
		Workdirs:      []string{base},
		Database:      baseDB,
		ProjectMaxLRU: 2,
	}
	defer s.closeProjectCache()

	var wg sync.WaitGroup
	workers := 10
	iterations := 20

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				targetProj := projs[(workerID+it)%numProjects]
				root, database, err := s.resolveProject(targetProj)
				if err != nil {
					t.Errorf("worker %d failed to resolve %s: %v", workerID, targetProj, err)
					return
				}
				if database == nil || root != targetProj {
					t.Errorf("worker %d got invalid result root=%s db=%v", workerID, root, database)
					return
				}
				// Simulate in-flight usage
				s.releaseProject(targetProj)
			}
		}(w)
	}
	wg.Wait()
}


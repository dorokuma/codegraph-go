package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
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

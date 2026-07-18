package sync

import (
	"testing"
	"time"
)

func TestIsSupported(t *testing.T) {
	ok := []string{"a.go", "b.ts", "c.tsx", "d.js", "e.py", "f.rs", "g.java", "h.swift"}
	for _, f := range ok {
		if !IsSupported(f) {
			t.Errorf("expected supported: %s", f)
		}
	}
	bad := []string{"a.md", "b.css", "c.json", "d.txt"}
	for _, f := range bad {
		if IsSupported(f) {
			t.Errorf("expected unsupported: %s", f)
		}
	}
}

func TestPendingFilesEmpty(t *testing.T) {
	w := &Watcher{pending: map[string]time.Time{}, workdir: "/tmp"}
	if got := w.PendingFiles(); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

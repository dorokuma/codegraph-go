//go:build !linux

package db

import (
	"os"
	"path/filepath"
	"testing"
)

// Non-Linux: /proc/self/fd magic links do not exist (macOS, the BSDs, and
// non-unix platforms alike), so SQLite keeps receiving the validated plain
// path. The dirfd pin plus recheckDir/recheckChild stay the detection layer.
func TestPinnedDBPathUsesPlainPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codegraph")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := pinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	want := filepath.Join(dir, "codegraph.db")
	got, gerr := pinnedDBPath(p, dir)
	if gerr != nil || got != want {
		t.Fatalf("pinnedDBPath = %q, %v, want %q", got, gerr, want)
	}
}

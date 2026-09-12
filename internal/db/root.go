package db

import (
	"os"
	"path/filepath"
)

// IsIndexed reports whether dir has a real .codegraph data directory
// (same idea as official isInitialized).
//
// The check uses Lstat, not Stat: a symlinked .codegraph does NOT count as
// an index. Its target is not this project's index, and treating a link as a
// valid root made FindNearestCodeGraphRoot select it (and db.Open populate
// the target with codegraph.db/-wal/-shm/lock) outside the project
// (adversarial-audit finding). Lstat never follows the final component, so
// a link is skipped and walk-up keeps climbing toward a real root.
func IsIndexed(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Lstat(filepath.Join(dir, ".codegraph"))
	return err == nil && st.IsDir()
}

// FindNearestCodeGraphRoot walks up from startPath looking for a directory
// that contains a real .codegraph/ (official findNearestCodeGraphRoot).
// A symlinked .codegraph does not count (see IsIndexed). Returns "" when
// none is found.
func FindNearestCodeGraphRoot(startPath string) string {
	if startPath == "" {
		return ""
	}
	current, err := filepath.Abs(startPath)
	if err != nil {
		current = filepath.Clean(startPath)
	}
	// If startPath is a file, begin at its directory.
	if st, err := os.Stat(current); err == nil && !st.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		if IsIndexed(current) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

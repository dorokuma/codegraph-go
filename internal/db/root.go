package db

import (
	"os"
	"path/filepath"
)

// IsIndexed reports whether dir has a .codegraph data directory
// (same idea as official isInitialized).
func IsIndexed(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, ".codegraph"))
	return err == nil && st.IsDir()
}

// FindNearestCodeGraphRoot walks up from startPath looking for a directory
// that contains .codegraph/ (official findNearestCodeGraphRoot).
// Returns "" when none is found.
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

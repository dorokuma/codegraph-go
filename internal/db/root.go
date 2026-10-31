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
	return FindNearestCodeGraphRootBounded(startPath, nil)
}

// FindNearestCodeGraphRootBounded is FindNearestCodeGraphRoot with an
// optional exclusion barrier between the candidate root and startPath.
//
// When isSkippedBetween is non-nil and a candidate root cand (holding a
// .codegraph dir) is found above startPath, every intermediate child path
// step on the way from cand down to startPath is passed to
// isSkippedBetween(cand, step, base(step)). A single true answer means
// startPath lives inside a subtree cand excludes (e.g. a .wt worktree
// container or an exclude_dirs rule): cand's index does not contain
// startPath, so cand is NOT adopted and the climb stops right there —
// returning "" instead of adopting a higher ancestor whose index is
// equally blind to startPath. nil preserves the unbounded behaviour.
func FindNearestCodeGraphRootBounded(startPath string, isSkippedBetween func(ancestor, childPath, childName string) bool) string {
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
	startDir := current
	for {
		if IsIndexed(current) {
			if isSkippedBetween == nil || current == startDir {
				return current
			}
			// Collect the intermediate steps from current down to startDir
			// (excluding current itself, including startDir).
			var steps []string
			for step := startDir; step != current; step = filepath.Dir(step) {
				steps = append(steps, step)
			}
			skipped := false
			for _, step := range steps {
				if isSkippedBetween(current, step, filepath.Base(step)) {
					skipped = true
					break
				}
			}
			if skipped {
				// startPath is inside a subtree this root excludes — its index
				// cannot contain startPath. Do not adopt this root, and do not
				// climb to a still-higher ancestor (same blindness).
				return ""
			}
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

package sync

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/extraction"
)

// GitDirtySourceFiles returns absolute paths of tracked source files that git
// reports as modified/added/deleted in the worktree. Best-effort: missing git
// or non-repo roots yield nil. Used after cold index so edits made while no
// watcher was running still get picked up (no git hooks installed).
func GitDirtySourceFiles(workdir string) []string {
	cmd := exec.Command("git", "-c", "core.quotePath=false", "-C", workdir, "status", "--porcelain", "--untracked-files=no")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		// XY<path>  or XY orig -> path (renames). For renames BOTH paths are
		// dirty: the old path must be dropped from the index and the new one
		// indexed (IndexChanges deletes old paths via os.Stat failure — A6).
		pathPart := strings.TrimSpace(line[2:])
		oldPath := ""
		if i := strings.Index(pathPart, " -> "); i >= 0 {
			oldPath = strings.TrimSpace(pathPart[:i])
			pathPart = pathPart[i+4:]
		}
		for _, p := range []string{oldPath, pathPart} {
			p = strings.Trim(p, "\"")
			p = strings.ReplaceAll(p, "\\\"", "\"")
			if p == "" {
				continue
			}
			abs := p
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(workdir, p)
			}
			abs = filepath.Clean(abs)
			if _, ok := seen[abs]; ok {
				continue
			}
			if !IsSupported(abs) {
				continue
			}
			// Skip dependency/generated trees.
			baseWalk := abs
			skip := false
			for {
				dir := filepath.Dir(baseWalk)
				if dir == baseWalk || dir == workdir {
					break
				}
				if extraction.ShouldSkipDirIn(workdir, dir, filepath.Base(dir)) {
					skip = true
					break
				}
				baseWalk = dir
			}
			if skip {
				continue
			}
			seen[abs] = struct{}{}
			files = append(files, abs)
		}
	}
	return files
}

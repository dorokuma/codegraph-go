package sync

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dorokuma/codegraph-go/extraction"
)

// GitDirtySourceFiles returns absolute paths of tracked source files that git
// reports as modified/added/deleted in the worktree. Best-effort: missing git
// or non-repo roots yield nil. Used after cold index so edits made while no
// watcher was running still get picked up (no git hooks installed).
func GitDirtySourceFiles(workdir string) []string {
	cmd := exec.Command("git", "-C", workdir, "status", "--porcelain", "--untracked-files=no")
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
		// XY<path>  or XY orig -> path (renames)
		pathPart := strings.TrimSpace(line[2:])
		if i := strings.Index(pathPart, " -> "); i >= 0 {
			pathPart = pathPart[i+4:]
		}
		pathPart = strings.Trim(pathPart, "\"")
		if pathPart == "" {
			continue
		}
		abs := pathPart
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workdir, pathPart)
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
	return files
}

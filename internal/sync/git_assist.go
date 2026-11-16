package sync

import (
	"context"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dorokuma/codegraph-go/internal/extraction"
)

// gitStatusTimeout bounds how long a git status probe may run. A hung git
// (stuck index lock, slow network filesystem) must not block cold-start sync
// forever; CommandContext kills the process when the timeout fires.
const gitStatusTimeout = 10 * time.Second

// GitDirtySourceFiles returns absolute paths of tracked source files that git
// reports as modified/added/deleted in the worktree. Best-effort: missing git
// or non-repo roots yield nil (callers treat that as clean). Used after cold
// index so edits made while no watcher was running still get picked up (no
// git hooks installed).
//
// A probe failure is NOT silently indistinguishable from a clean tree: the
// cause is logged so an assist that stopped working (git missing, non-repo,
// permission, timeout) is visible in the logs instead of masquerading as
// "no dirty files".
func GitDirtySourceFiles(workdir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), gitStatusTimeout)
	defer cancel()

	// Porcelain paths are relative to the REPOSITORY ROOT, not to the -C
	// directory: with workdir=<repoRoot>/sub, `git status` emits "sub/foo.go",
	// so joining onto workdir would produce <repoRoot>/sub/sub/foo.go and would
	// also swallow dirty siblings outside the indexed subtree. Resolve the
	// toplevel first, then join + filter below.
	rootCmd := exec.CommandContext(ctx, "git", "-C", workdir, "rev-parse", "--show-toplevel")
	rootOut, err := rootCmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("git-assist: git rev-parse timed out after %s in %s", gitStatusTimeout, workdir)
		} else {
			log.Printf("git-assist: git rev-parse failed in %s: %v", workdir, err)
		}
		return nil
	}
	repoRoot := strings.TrimSpace(string(rootOut))
	if repoRoot == "" {
		log.Printf("git-assist: git rev-parse returned empty toplevel in %s", workdir)
		return nil
	}
	if !filepath.IsAbs(repoRoot) {
		repoRoot = filepath.Join(workdir, repoRoot)
	}

	// Align the comparison base: callers pass a canonicalized workdir
	// (Abs+EvalSymlinks in cmd) while --show-toplevel prints the physical
	// path. If either side still differs (uncanonicalized caller, linked
	// checkout), EvalSymlinks both sides before the subtree test so files
	// are not wrongly dropped — and the joined absolute path is computed
	// against the same canonical root it is compared to.
	canonical := func(p string) string {
		if rp, err := filepath.EvalSymlinks(p); err == nil && rp != "" {
			return rp
		}
		return filepath.Clean(p)
	}
	workdir = canonical(workdir)
	repoRoot = canonical(repoRoot)

	cmd := exec.CommandContext(ctx, "git", "-c", "core.quotePath=false", "-C", workdir, "status", "--porcelain", "--untracked-files=no")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("git-assist: git status timed out after %s in %s", gitStatusTimeout, workdir)
		} else {
			log.Printf("git-assist: git status failed in %s: %v", workdir, err)
		}
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
				abs = filepath.Join(repoRoot, p)
			}
			abs = filepath.Clean(abs)
			// Porcelain output spans the whole repository; drop entries
			// outside the indexed workdir subtree.
			if rel, rerr := filepath.Rel(workdir, abs); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
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

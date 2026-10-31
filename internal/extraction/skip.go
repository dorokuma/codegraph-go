package extraction

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/config"
)

// Directory basenames that are never project source for indexing/watching.
// Expanding this list does not remove any MCP tools — it only avoids scanning
// dependency caches and build outputs that blow up cold-start time.
//
// Go module/toolchain trees (HOME/go/pkg, etc.) are handled by isGoToolchainPath
// with path-segment matching so project dirs like "codegraph-go/pkg" stay indexed.
var skipDirNames = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, ".codegraph": {},
	"dist": {}, "build": {}, "out": {}, "coverage": {},
	"__pycache__": {}, ".venv": {}, "venv": {}, "target": {},
	".gradle": {}, ".cargo": {}, ".rustup": {}, ".npm": {}, ".nvm": {},
	".cache": {}, ".pub-cache": {}, ".dartServer": {}, ".android": {},
	".tox": {}, ".mypy_cache": {}, ".pytest_cache": {}, ".ruff_cache": {},
	".next": {}, ".nuxt": {}, ".turbo": {}, ".yarn": {}, "bower_components": {},
	"site-packages": {}, ".eggs": {}, ".idea": {}, ".terraform": {},
	"Pods": {}, "Carthage": {}, ".bundle": {}, "DerivedData": {},
	// .wt is the git-worktree container dir used by dispatched sub-agents
	// (/root/workspace/.wt/<repo>/<branch>); each child worktree is a
	// duplicate of a project that is already indexed under the workspace
	// root, so walking it again doubles cold-start cost for zero new code.
	".wt": {},
}

// Path fragments (slash-normalized) that mark cache/toolchain trees.
// Matched as substring of the absolute path so nested layouts are covered.
// Do NOT put "/go/pkg/" here — it false-positives on projects named "*-go/pkg".
var skipPathFragments = []string{
	"/.gradle/",
	"/.cargo/registry/",
	"/.cargo/git/",
	"/.rustup/",
	"/.npm/",
	"/.nvm/",
	"/.cache/",
	"/.pub-cache/",
	"/node_modules/",
	"/.local/share/uv/",
	"/.local/share/virtualenv/",
	"/.local/lib/",
	"/.local/share/",
	"/Library/Caches/",
	"/.gradle/caches/",
	"/AppData/Local/Go/",
	"/AppData/Local/npm-cache/",
	// Bulk non-source dumps often present under home when ~ is indexed by mistake
	"/code_references/",
	"/gopath/pkg/",
	"/gopath/src/golang.org/",
}

// Markers that mean "this top-level folder is a real project" when workdir is $HOME.
var projectMarkers = []string{
	".git",
	"go.mod",
	"package.json",
	"pyproject.toml",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"composer.json",
	"Gemfile",
	"mix.exs",
	"CMakeLists.txt",
	"Makefile",
	"Cargo.lock",
	"pnpm-workspace.yaml",
	"setup.py",
	"requirements.txt",
	"Pipfile",
	"build.sbt",
	"Package.swift",
}

// isGoToolchainPath reports GOPATH / module-cache style trees:
//
//	.../go/pkg/...   .../go/bin/...   .../go/src/...
//
// Uses path segments so "codegraph-go/pkg" is NOT treated as GOPATH.
func isGoToolchainPath(slash string) bool {
	parts := strings.Split(slash, "/")
	for i := 0; i < len(parts); i++ {
		if parts[i] != "go" {
			continue
		}
		if i+1 >= len(parts) {
			// path ends at .../go — skip only known GOPATH roots, not
			// project dirs named "go" (sdk/go, internal/go, clients/go).
			return isGopathRoot(slash)
		}
		next := parts[i+1]
		if next == "pkg" || next == "bin" || next == "src" {
			return true
		}
	}
	return false
}

// isGopathRoot reports whether slash-normalized path is a GOPATH workspace
// root: /root/go, $HOME/go, /home/<user>/go, or /Users/<user>/go.
func isGopathRoot(slash string) bool {
	slash = strings.TrimSuffix(slash, "/")
	if slash == "/root/go" {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil {
		if slash == filepath.ToSlash(filepath.Join(home, "go")) {
			return true
		}
	}
	parts := strings.Split(slash, "/")
	// "/home/u/go" → ["", "home", "u", "go"]
	if len(parts) == 4 && (parts[1] == "home" || parts[1] == "Users") && parts[2] != "" && parts[3] == "go" {
		return true
	}
	return false
}

// HasProjectMarker reports whether dir looks like a software project root.
func HasProjectMarker(dir string) bool {
	for _, m := range projectMarkers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// IsBroadWorkdir is true for $HOME and multi-user roots where a full recursive
// index would suck in toolchains and caches. Callers use home-mode filtering.
func IsBroadWorkdir(workdir string) bool {
	if workdir == "" {
		return false
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		abs = workdir
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return true
	}
	// Multi-user roots (not a single user's home)
	slash := filepath.ToSlash(abs)
	if slash == "/home" || slash == "/Users" {
		return true
	}
	home, err := os.UserHomeDir()
	if err == nil && filepath.Clean(home) == abs {
		return true
	}
	// Explicit server home
	if slash == "/root" {
		return true
	}
	return false
}

// homeIndexAll enables indexing every non-cache top-level dir under $HOME.
// Default is off: only project-like top-level dirs are entered.
func homeIndexAll() bool {
	return config.HomeIndexAll()
}

// NarrowExcludedWorkdirs is the single source of truth for the startup
// workdir narrowing: when the session target — an explicit -workdir the
// user passed (explicitWorkdir, takes precedence) or the process cwd —
// sits inside a configured workdir's subtree that the parent root
// refuses to index (a built-in skip name like .wt, an exclude_dirs rule,
// or a home-mode non-project top level), the parent workdir's index is
// blind to that subtree by construction. Returning []string{target}
// shrinks the workdir list to the target itself so the session indexes
// its own tree instead of a blind parent index — and does NOT keep the
// parent as a secondary (ExtraDBs) workdir.
//
// The rule semantics are NOT re-implemented here: every intermediate
// path segment between the workdir root and the target is checked with
// ShouldSkipDirIn(wdAbs, segPath, basename) — the same function the
// indexer's walker applies — so any layer the walk would skip triggers
// the narrowing. ShouldSkipDirIn's root protection makes the check
// safe: a target equal to the workdir (or reached through a protected
// intermediate) never narrows.
//
// explicitWorkdir == "" means the caller did not pass -workdir, so cwd
// is the target; when cwd is itself a configured workdir nothing
// narrows. An explicit target equal to a workdir root (or outside every
// configured root) likewise leaves the list untouched. workdirs is
// returned unchanged when the target is empty or the list is empty.
func NarrowExcludedWorkdirs(explicitWorkdir, cwd string, workdirs []string) []string {
	target := explicitWorkdir
	if target == "" {
		target = cwd
	}
	if target == "" || len(workdirs) == 0 {
		return workdirs
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return workdirs
	}
	targetAbs = filepath.Clean(targetAbs)
	for _, wd := range workdirs {
		wdAbs, err := filepath.Abs(wd)
		if err != nil {
			continue
		}
		wdAbs = filepath.Clean(wdAbs)
		if wdAbs == targetAbs {
			continue // target IS a configured workdir: nothing to narrow
		}
		rel, err := filepath.Rel(wdAbs, targetAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		segPath := wdAbs
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if seg == "" || seg == "." {
				continue
			}
			segPath = filepath.Join(segPath, seg)
			if ShouldSkipDirIn(wdAbs, segPath, filepath.Base(segPath)) {
				return []string{target}
			}
		}
	}
	return workdirs
}

// ShouldSkipDir reports whether a directory should be skipped during walk.
// name is the base name; path is the full path (absolute or relative).
// Prefer ShouldSkipDirIn when the workspace root is known (enables home-mode).
func ShouldSkipDir(path, name string) bool {
	return ShouldSkipDirIn("", path, name)
}

// ShouldSkipDirIn is like ShouldSkipDir but applies home-mode rules when
// workdir is $HOME / broad: only descend into top-level dirs that look like
// projects (go.mod, package.json, .git, …), unless CODEGRAPH_GO_HOME_INDEX_ALL=1.
//
// With a known workdir it also applies the user-configured exclusions
// (config.yaml exclude_dirs / CODEGRAPH_EXCLUDE_DIRS): bare names and globs
// match the directory's base name; patterns containing '/' match the
// workdir-relative slash path; patterns anchored with a leading "./" or "/"
// (e.g. "./ref") pin the exclusion to <workdir>/<sub> and its subtree only
// instead of hitting a same-named deep directory. The workdir root itself is
// never skipped, even when its own base name matches a rule (an agent may
// point the workdir at a single worktree).
//
// This lets users keep workdir=/root (their real workspace) without indexing
// GOPATH, package caches, random dumps, and dotfile trees.
func ShouldSkipDirIn(workdir, path, name string) bool {
	if name == "" {
		name = filepath.Base(path)
	}
	// Normalize workdir and path to absolute + clean once so relative
	// comparisons (root protection, exclude_dirs rel rules) are consistent
	// even when callers hand us "." or mixed relative/absolute paths.
	var workdirAbs, pathAbs string
	if workdir != "" {
		wAbs, err := filepath.Abs(workdir)
		if err != nil {
			wAbs = workdir
		}
		workdirAbs = filepath.Clean(wAbs)
		pAbs, err := filepath.Abs(path)
		if err != nil {
			pAbs = path
		}
		pathAbs = filepath.Clean(pAbs)
	}
	// Root protection: the workdir root itself is never skipped. An agent
	// may legitimately point workdir at a single worktree whose own name
	// is listed in skipDirNames or matches an exclude_dirs rule — skipping
	// the root would blank-index the whole project.
	if workdir != "" && pathAbs == workdirAbs {
		return false
	}
	if _, ok := skipDirNames[name]; ok {
		return true
	}
	// User-configured exclusions (config.yaml exclude_dirs +
	// CODEGRAPH_EXCLUDE_DIRS). Each rule is either a bare directory name /
	// glob matched against the base name, or a workdir-relative slash
	// path matched against the path's rel from the workdir root.
	for _, pat := range config.ExcludeDirs() {
		pat = strings.TrimSuffix(strings.TrimSpace(pat), "/")
		if pat == "" {
			continue
		}
		if !strings.Contains(pat, "/") {
			if name == pat {
				return true
			}
			if ok, err := filepath.Match(pat, name); err == nil && ok {
				return true
			}
			continue
		}
		if workdirAbs == "" {
			continue
		}
		if strings.HasPrefix(pat, "./") || strings.HasPrefix(pat, "/") {
			// Top-level anchor: "./ref" (or "/ref") pins the exclusion to
			// <workdir>/ref and its subtree only — a bare "ref" would also
			// hit nested dirs like <workdir>/proj/internal/ref.
			sub := strings.TrimPrefix(strings.TrimPrefix(pat, "./"), "/")
			if sub == "" {
				continue
			}
			rel, err := filepath.Rel(workdirAbs, pathAbs)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if rel == sub || strings.HasPrefix(rel, sub+"/") {
				return true
			}
			if ok, mErr := filepath.Match(sub, rel); mErr == nil && ok {
				return true
			}
			continue
		}
		rel, err := filepath.Rel(workdirAbs, pathAbs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == pat || strings.HasPrefix(rel, pat+"/") {
			return true
		}
		if ok, mErr := filepath.Match(pat, rel); mErr == nil && ok {
			return true
		}
	}
	// Toolchain / cache path-fragment checks run on the workdir-RELATIVE
	// path when the target sits inside the workdir: a narrowed workdir
	// root like /root/go/pkg/mod/foo or /ws/node_modules/pkg must not
	// get its normal subdirs rejected just because an ANCESTOR segment
	// happened to contain "/go/pkg/" or "/node_modules/" — the parent
	// root's walk was already responsible for that exclusion. When
	// workdir is empty (ShouldSkipDir) or the path escapes it, the full
	// absolute path is checked as before.
	checkTarget := ""
	if workdirAbs != "" {
		if rel, err := filepath.Rel(workdirAbs, pathAbs); err == nil &&
			rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			checkTarget = "/" + strings.Trim(filepath.ToSlash(rel), "/")
			if isGoToolchainPath(checkTarget) ||
				(rel == "go" && isGopathRoot(filepath.ToSlash(pathAbs))) {
				return true
			}
			check := checkTarget + "/"
			for _, frag := range skipPathFragments {
				if strings.Contains(check, frag) {
					return true
				}
			}
		}
	}
	if checkTarget == "" {
		slash := filepath.ToSlash(path)
		if !strings.HasPrefix(slash, "/") && !strings.Contains(slash, ":/") {
			slash = "/" + slash
		}
		trimmed := strings.TrimSuffix(slash, "/")
		if isGoToolchainPath(trimmed) {
			return true
		}
		check := slash
		if !strings.HasSuffix(check, "/") {
			check += "/"
		}
		for _, frag := range skipPathFragments {
			if strings.Contains(check, frag) {
				return true
			}
		}
	}

	// Home / broad workdir: only index project-like top-level folders.
	if workdirAbs != "" && IsBroadWorkdir(workdir) && !homeIndexAll() {
		rel, err := filepath.Rel(workdirAbs, pathAbs)
		if err == nil {
			rel = filepath.ToSlash(rel)
			// Direct child of workdir only (top-level entry).
			if rel != "." && !strings.Contains(rel, "/") {
				// Hidden top-level dirs under home are almost never project roots.
				if strings.HasPrefix(name, ".") {
					return true
				}
				if !HasProjectMarker(pathAbs) {
					return true
				}
			}
		}
	}
	return false
}

// BuiltinSkipGlobs returns the indexer's built-in directory exclusions
// (skipDirNames + skipPathFragments + Go toolchain/GOPATH segments) as a
// deduplicated, lexically sorted list of gitignore-style glob strings.
// It is the single source of truth the rg exclusion args consume, so the
// rg fallback and the walker can never drift apart.
//
// Each entry carries the semantics rg needs — a leading "/" anchors the
// rule to the search root's top level, an unanchored multi-segment
// fragment must apply at any depth, and a bare name is directory-only.
// The caller maps each entry to a "!" negative glob pair (dir + "/**").
//
// The three shapes emitted are:
//
//	name        bare directory basename   (node_modules, .cache, …)
//	**/a/b      multi-segment fragment    (.local/lib, go/pkg, …)
//	/name       top-level anchored dir    (./ref, /go under a GOPATH root)
//
// root is the workdir/project root. It is only used to decide whether the
// top-level "go" directory is itself a GOPATH workspace (isGopathRoot on
// <root>/go) — e.g. workdir=/root, ~, /home/u or /Users/u. All other
// rules are global.
func BuiltinSkipGlobs(root string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(g string) {
		if g == "" {
			return
		}
		if _, ok := seen[g]; ok {
			return
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}

	// a) Bare directory basenames — a same-named dir at ANY depth is
	// excluded, but never a same-named plain file (gitignore "dir/").
	for name := range skipDirNames {
		add(name)
	}
	// Single-segment fragments reduce to bare names too
	// (e.g. "/code_references/" -> "code_references").
	for _, frag := range skipPathFragments {
		f := strings.Trim(frag, "/")
		if f != "" && !strings.Contains(f, "/") {
			add(f)
		}
	}

	// b) Multi-segment path fragments — unanchored "**/a/b" so the
	// fragment matches at the root top level AND at any depth below it.
	for _, frag := range skipPathFragments {
		f := strings.Trim(frag, "/")
		if f != "" && strings.Contains(f, "/") {
			add("**/" + f)
		}
	}
	// Go toolchain segments (.../go/pkg, .../go/bin, .../go/src) — the
	// isGoToolchainPath rule, as glob fragments. A leading "go" path
	// element is required so "codegraph-go/pkg" or "sdk/go" is never hit.
	for _, seg := range []string{"go/pkg", "go/bin", "go/src"} {
		add("**/" + seg)
	}

	// c) Top-level anchored GOPATH dir: only when <root>/go would itself
	// be a GOPATH workspace root (root is /root, $HOME, /home/<u> or
	// /Users/<u>) — the "rel == go && isGopathRoot" walker case.
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			abs = root
		}
		if isGopathRoot(filepath.ToSlash(filepath.Join(filepath.Clean(abs), "go"))) {
			add("/go")
		}
	}

	sort.Strings(out)
	return out
}

package extraction

import (
	"os"
	"path/filepath"
	"strings"
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
			// path ends at .../go — whole GOPATH root
			return true
		}
		next := parts[i+1]
		if next == "pkg" || next == "bin" || next == "src" {
			return true
		}
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
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CODEGRAPH_GO_HOME_INDEX_ALL")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
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
// This lets users keep workdir=/root (their real workspace) without indexing
// GOPATH, package caches, random dumps, and dotfile trees.
func ShouldSkipDirIn(workdir, path, name string) bool {
	if name == "" {
		name = filepath.Base(path)
	}
	if _, ok := skipDirNames[name]; ok {
		return true
	}
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

	// Home / broad workdir: only index project-like top-level folders.
	if workdir != "" && IsBroadWorkdir(workdir) && !homeIndexAll() {
		wAbs, err := filepath.Abs(workdir)
		if err != nil {
			wAbs = workdir
		}
		pAbs, err := filepath.Abs(path)
		if err != nil {
			pAbs = path
		}
		rel, err := filepath.Rel(filepath.Clean(wAbs), filepath.Clean(pAbs))
		if err == nil {
			rel = filepath.ToSlash(rel)
			// Direct child of workdir only (top-level entry).
			if rel != "." && !strings.Contains(rel, "/") {
				// Hidden top-level dirs under home are almost never project roots.
				if strings.HasPrefix(name, ".") {
					return true
				}
				if !HasProjectMarker(pAbs) {
					return true
				}
			}
		}
	}
	return false
}

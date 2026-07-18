package extraction

import (
	"path/filepath"
	"strings"
)

// Directory basenames that are never project source for indexing/watching.
// Expanding this list does not remove any MCP tools — it only avoids scanning
// dependency caches and build outputs that blow up cold-start time.
//
// Go module/toolchain trees (go/pkg/mod, etc.) are handled only via
// skipPathFragments so ordinary project dirs named "pkg" or "mod" stay indexed.
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
var skipPathFragments = []string{
	"/go/pkg/mod/",
	"/go/pkg/sumdb/",
	"/go/pkg/tool/",
	"/gopath/pkg/",
	"/gopath/src/golang.org/",
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
	"/Library/Caches/",
	"/.gradle/caches/",
	"/AppData/Local/Go/",
	"/AppData/Local/npm-cache/",
}

// ShouldSkipDir reports whether a directory should be skipped during walk.
// name is the base name; path is the full path (absolute or relative).
func ShouldSkipDir(path, name string) bool {
	if _, ok := skipDirNames[name]; ok {
		return true
	}
	slash := filepath.ToSlash(path)
	if !strings.HasPrefix(slash, "/") && !strings.Contains(slash, ":/") {
		// relative path — still check fragments with leading slash form
		slash = "/" + slash
	}
	if !strings.HasSuffix(slash, "/") {
		slash += "/"
	}
	for _, frag := range skipPathFragments {
		if strings.Contains(slash, frag) {
			return true
		}
	}
	return false
}

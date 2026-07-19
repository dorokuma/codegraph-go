package resolution

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// jsExts is the extension/index try-list for TS/JS (and friends).
var jsExts = []string{
	".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".d.ts",
	"/index.ts", "/index.tsx", "/index.js", "/index.jsx",
}

// ResolveImportPath turns a relative/module/aliased import specifier into
// candidate files under workdir. Supports Go module+replace, tsconfig paths,
// package workspaces, cargo workspace crates, and conventional @/ fallbacks.
func ResolveImportPath(workdir, fromFile, spec, lang string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	fromDir := filepath.Dir(fromFile)
	workdir = filepath.Clean(workdir)

	switch lang {
	case "go":
		return resolveGoImportPath(workdir, fromDir, spec)
	case "typescript", "javascript", "tsx", "jsx":
		return resolveJSImportPath(workdir, fromDir, spec)
	case "python":
		return resolvePythonImportPath(workdir, fromDir, spec)
	case "rust":
		return resolveRustImportPath(workdir, fromFile, spec)
	default:
		if strings.HasPrefix(spec, ".") {
			return expandWithExt(filepath.Join(fromDir, spec), nil)
		}
		// Still try alias/workspace for vue/svelte/etc.
		if hit := resolveJSImportPath(workdir, fromDir, spec); len(hit) > 0 {
			return hit
		}
		// Rust-like paths sometimes show up with empty/unknown lang.
		if strings.Contains(spec, "::") {
			return resolveRustImportPath(workdir, fromFile, spec)
		}
		return nil
	}
}

func resolveRustImportPath(workdir, fromFile, spec string) []string {
	// Relative path rare in Rust; still honor ./foo.rs style if present.
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		base := filepath.Join(filepath.Dir(fromFile), spec)
		return expandWithExt(base, []string{".rs", "/mod.rs"})
	}
	return ResolveCargoImport(workdir, fromFile, spec)
}

func resolveGoImportPath(workdir, fromDir, spec string) []string {
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		return expandWithExt(filepath.Join(fromDir, spec), []string{".go"})
	}
	mod := LoadGoModule(workdir)
	if mod == nil {
		// Walk up from fromDir in case go.mod sits above a nested package
		// but workdir is broader (home mode). Prefer nearest go.mod.
		mod = loadNearestGoModule(fromDir, workdir)
	}
	if mod == nil {
		return nil
	}
	pkgDir := ResolveGoImport(mod, spec)
	if pkgDir == "" {
		return nil
	}
	files := ListGoPackageFiles(pkgDir)
	return uniqueExisting(files)
}

func loadNearestGoModule(fromDir, stopAt string) *GoModule {
	cur := filepath.Clean(fromDir)
	stopAt = filepath.Clean(stopAt)
	for {
		if m := LoadGoModule(cur); m != nil {
			return m
		}
		if cur == stopAt {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		// Don't escape stopAt
		if stopAt != "" && cur != stopAt && !strings.HasPrefix(cur, stopAt+string(filepath.Separator)) {
			break
		}
		cur = parent
	}
	return nil
}

func resolveJSImportPath(workdir, fromDir, spec string) []string {
	// Relative
	if strings.HasPrefix(spec, ".") {
		base := filepath.Join(fromDir, spec)
		return expandWithExt(base, jsExts)
	}

	// 1. tsconfig/jsconfig paths
	if aliases := LoadProjectAliases(workdir); aliases != nil {
		for _, rel := range ApplyAliases(spec, aliases, workdir) {
			base := filepath.Join(workdir, filepath.FromSlash(rel))
			if hit := expandWithExt(base, jsExts); len(hit) > 0 {
				return hit
			}
		}
	}
	// Also try nearest tsconfig above fromDir (monorepo package-level config).
	if near := findNearestConfigDir(fromDir, workdir, "tsconfig.json", "jsconfig.json"); near != "" && near != workdir {
		if aliases := LoadProjectAliases(near); aliases != nil {
			for _, rel := range ApplyAliases(spec, aliases, near) {
				base := filepath.Join(near, filepath.FromSlash(rel))
				if hit := expandWithExt(base, jsExts); len(hit) > 0 {
					return hit
				}
			}
		}
	}

	// 2. workspace packages (@scope/ui → packages/ui)
	if ws := LoadWorkspacePackages(workdir); ws != nil {
		if rel := ResolveWorkspaceImport(spec, ws); rel != "" {
			base := filepath.Join(workdir, filepath.FromSlash(rel))
			if hit := expandWithExt(base, jsExts); len(hit) > 0 {
				return hit
			}
		}
	}

	// 3. Conventional fallbacks (@/ → src/)
	if fb := ApplyFallbackAliases(spec); fb != "" {
		base := filepath.Join(workdir, filepath.FromSlash(fb))
		if hit := expandWithExt(base, jsExts); len(hit) > 0 {
			return hit
		}
	}

	// 4. Direct path under workdir
	base := filepath.Join(workdir, filepath.FromSlash(spec))
	return expandWithExt(base, jsExts)
}

func findNearestConfigDir(fromDir, stopAt string, names ...string) string {
	cur := filepath.Clean(fromDir)
	stopAt = filepath.Clean(stopAt)
	for {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(cur, n)); err == nil {
				return cur
			}
		}
		if cur == stopAt {
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func resolvePythonImportPath(workdir, fromDir, spec string) []string {
	if strings.HasPrefix(spec, ".") {
		rel := strings.TrimLeft(spec, ".")
		parts := strings.Split(rel, ".")
		base := fromDir
		// leading dots beyond one walk up parents
		ups := strings.Count(spec, ".") - strings.Count(rel, ".") - 1
		if ups < 0 {
			ups = 0
		}
		for i := 0; i < ups && filepath.Dir(base) != base; i++ {
			base = filepath.Dir(base)
		}
		p := filepath.Join(append([]string{base}, parts...)...)
		return expandWithExt(p, []string{".py", "/__init__.py"})
	}
	base := filepath.Join(fromDir, filepath.Join(strings.Split(spec, ".")...))
	cands := expandWithExt(base, []string{".py", "/__init__.py"})
	rootBase := filepath.Join(workdir, filepath.Join(strings.Split(spec, ".")...))
	cands = append(cands, expandWithExt(rootBase, []string{".py", "/__init__.py"})...)
	return uniqueExisting(cands)
}

func expandWithExt(base string, suffixes []string) []string {
	var out []string
	if st, err := os.Stat(base); err == nil && !st.IsDir() {
		out = append(out, base)
	}
	for _, s := range suffixes {
		p := base
		if strings.HasPrefix(s, "/") {
			p = base + s
		} else if s != "" {
			p = base + s
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, p)
		}
	}
	// directory → index files already covered by /index.* suffixes;
	// also accept any file directly inside when base is a dir with single candidate? skip.
	return uniqueExisting(out)
}

func uniqueExisting(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// FilterByImports prefers candidates living in files reached via this file's imports.
// Returns (filtered, true) when import closure yields a non-empty subset.
func FilterByImports(database *db.DB, workdir, fromFile, lang string, candidates []db.Node) ([]db.Node, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	specs, err := database.GetImportTargetNames(fromFile)
	if err != nil || len(specs) == 0 {
		return candidates, false
	}
	importedFiles := map[string]bool{}
	for _, spec := range specs {
		for _, p := range ResolveImportPath(workdir, fromFile, spec, lang) {
			importedFiles[p] = true
		}
		// Also accept module path suffix match against candidate.File
		for _, c := range candidates {
			if strings.Contains(c.File, filepath.ToSlash(spec)) ||
				strings.HasSuffix(filepath.Dir(c.File), spec) ||
				strings.TrimSuffix(filepath.Base(c.File), filepath.Ext(c.File)) == filepath.Base(spec) {
				importedFiles[c.File] = true
			}
		}
	}
	if len(importedFiles) == 0 {
		return candidates, false
	}
	var hit []db.Node
	for _, c := range candidates {
		if importedFiles[c.File] {
			hit = append(hit, c)
		}
	}
	if len(hit) == 0 {
		return candidates, false
	}
	return hit, true
}

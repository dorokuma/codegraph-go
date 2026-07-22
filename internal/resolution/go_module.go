package resolution

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// GoModule is the project-root go.mod view (official go-module.ts + replace).
type GoModule struct {
	ModulePath string            // e.g. github.com/example/myproject
	RootDir    string            // absolute dir containing go.mod
	Replace    map[string]string // module path prefix → absolute local dir
}

var (
	goModCacheMu sync.Mutex
	goModCache   = map[string]*goModCacheEntry{}
)

type goModCacheEntry struct {
	modTime int64
	mod     *GoModule
}

// ClearGoModuleCache drops cached go.mod parses.
func ClearGoModuleCache() {
	goModCacheMu.Lock()
	defer goModCacheMu.Unlock()
	goModCache = map[string]*goModCacheEntry{}
}

var (
	goModuleRe  = regexp.MustCompile(`(?m)^\s*module\s+(\S+)\s*$`)
	goReplaceRe = regexp.MustCompile(`^\s*([^\s]+)(?:\s+v[^\s]+)?\s*=>\s*([^\s]+)(?:\s+v[^\s]+)?\s*$`)
)

// LoadGoModule reads go.mod at projectRoot. Returns nil when absent/invalid.
func LoadGoModule(projectRoot string) *GoModule {
	projectRoot = filepath.Clean(projectRoot)
	p := filepath.Join(projectRoot, "go.mod")
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	mtime := st.ModTime().UnixNano()
	goModCacheMu.Lock()
	if e, ok := goModCache[projectRoot]; ok && e.modTime == mtime {
		m := e.mod
		goModCacheMu.Unlock()
		return m
	}
	goModCacheMu.Unlock()

	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	mod := parseGoMod(projectRoot, string(raw))
	goModCacheMu.Lock()
	goModCache[projectRoot] = &goModCacheEntry{modTime: mtime, mod: mod}
	goModCacheMu.Unlock()
	return mod
}

func parseGoMod(projectRoot, content string) *GoModule {
	// Strip // line comments (keep strings simple — go.mod rarely has // in paths).
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	stripped := b.String()

	m := goModuleRe.FindStringSubmatch(stripped)
	if m == nil {
		return nil
	}
	modulePath := strings.Trim(m[1], `"'`)
	if modulePath == "" {
		return nil
	}
	gm := &GoModule{
		ModulePath: modulePath,
		RootDir:    projectRoot,
		Replace:    map[string]string{},
	}

	// Parse replace directives (single-line and block).
	lines := strings.Split(stripped, "\n")
	inReplace := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "replace (") {
			inReplace = true
			continue
		}
		if inReplace {
			if trim == ")" {
				inReplace = false
				continue
			}
			parseReplaceLine(gm, trim)
			continue
		}
		if strings.HasPrefix(trim, "replace ") {
			parseReplaceLine(gm, strings.TrimSpace(strings.TrimPrefix(trim, "replace")))
		}
	}
	return gm
}

func parseReplaceLine(gm *GoModule, line string) {
	// forms: old [v] => new [v]   |  old [v] => ../local
	mm := goReplaceRe.FindStringSubmatch(line)
	if mm == nil {
		return
	}
	oldPath := strings.Trim(mm[1], `"'`)
	newPath := strings.Trim(mm[2], `"'`)
	if oldPath == "" || newPath == "" {
		return
	}
	// Only local filesystem replacements (./ or ../ or absolute).
	if !strings.HasPrefix(newPath, ".") && !filepath.IsAbs(newPath) {
		// module path → other module version; not a local dir we can open
		return
	}
	var abs string
	if filepath.IsAbs(newPath) {
		abs = filepath.Clean(newPath)
	} else {
		abs = filepath.Clean(filepath.Join(gm.RootDir, newPath))
	}
	gm.Replace[oldPath] = abs
}

// ResolveGoImport maps an import path to a local package directory, or "".
func ResolveGoImport(mod *GoModule, importPath string) string {
	if mod == nil || importPath == "" {
		return ""
	}
	// Longest replace prefix wins.
	bestOld := ""
	bestDir := ""
	for old, dir := range mod.Replace {
		if importPath == old || strings.HasPrefix(importPath, old+"/") {
			if len(old) > len(bestOld) {
				bestOld = old
				bestDir = dir
			}
		}
	}
	if bestOld != "" {
		rest := strings.TrimPrefix(importPath, bestOld)
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			return bestDir
		}
		return filepath.Join(bestDir, filepath.FromSlash(rest))
	}

	if importPath == mod.ModulePath {
		return mod.RootDir
	}
	if strings.HasPrefix(importPath, mod.ModulePath+"/") {
		rel := strings.TrimPrefix(importPath, mod.ModulePath+"/")
		return filepath.Join(mod.RootDir, filepath.FromSlash(rel))
	}
	return ""
}

// ListGoPackageFiles returns .go files directly in dir (not nested packages).
func ListGoPackageFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	// If only tests exist, still return them so resolution isn't empty.
	if len(out) == 0 {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".go") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out
}

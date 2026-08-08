package resolution

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// WorkspacePackages maps monorepo package names → project-relative dirs.
type WorkspacePackages struct {
	ByName map[string]string // name → dir relative to root (slash)
}

var (
	wsCacheMu sync.Mutex
	wsCache   = map[string]*wsCacheEntry{}
)

type wsCacheEntry struct {
	modTime int64
	ws      *WorkspacePackages
}

// ClearWorkspaceCache drops cached workspace maps.
func ClearWorkspaceCache() {
	wsCacheMu.Lock()
	defer wsCacheMu.Unlock()
	wsCache = map[string]*wsCacheEntry{}
}

// LoadWorkspacePackages reads package.json workspaces + pnpm-workspace.yaml.
// Also records the root package.json name → "." when present.
func LoadWorkspacePackages(projectRoot string) *WorkspacePackages {
	projectRoot = filepath.Clean(projectRoot)
	// Cache key mtime: max of package.json / pnpm-workspace.yaml
	var mtime int64
	for _, name := range []string{"package.json", "pnpm-workspace.yaml"} {
		if st, err := os.Stat(filepath.Join(projectRoot, name)); err == nil {
			if t := st.ModTime().UnixNano(); t > mtime {
				mtime = t
			}
		}
	}
	wsCacheMu.Lock()
	if e, ok := wsCache[projectRoot]; ok && e.modTime == mtime {
		w := e.ws
		wsCacheMu.Unlock()
		return w
	}
	wsCacheMu.Unlock()

	byName := map[string]string{}

	// Root package name
	if name := readPackageName(filepath.Join(projectRoot, "package.json")); name != "" {
		byName[name] = "."
	}

	for _, pattern := range readWorkspaceGlobs(projectRoot) {
		for _, dir := range expandWorkspaceGlob(projectRoot, pattern) {
			pkgName := readPackageName(filepath.Join(projectRoot, dir, "package.json"))
			if pkgName == "" {
				continue
			}
			if _, exists := byName[pkgName]; !exists {
				byName[pkgName] = filepath.ToSlash(dir)
			}
		}
	}

	var ws *WorkspacePackages
	if len(byName) > 0 {
		ws = &WorkspacePackages{ByName: byName}
	}
	wsCacheMu.Lock()
	wsCache[projectRoot] = &wsCacheEntry{modTime: mtime, ws: ws}
	wsCacheMu.Unlock()
	return ws
}

// ResolveWorkspaceImport rewrites @scope/ui/widgets → packages/ui/widgets.
// Returns project-relative slash path without extension, or "".
func ResolveWorkspaceImport(importPath string, ws *WorkspacePackages) string {
	if ws == nil || importPath == "" {
		return ""
	}
	var bestName string
	for name := range ws.ByName {
		if importPath == name || strings.HasPrefix(importPath, name+"/") {
			if len(name) > len(bestName) {
				bestName = name
			}
		}
	}
	if bestName == "" {
		return ""
	}
	dir := ws.ByName[bestName]
	sub := strings.TrimPrefix(importPath, bestName)
	sub = strings.TrimPrefix(sub, "/")
	if dir == "." {
		if sub == "" {
			return "."
		}
		return filepath.ToSlash(sub)
	}
	if sub == "" {
		return dir
	}
	return filepath.ToSlash(filepath.Join(dir, sub))
}

func readPackageName(pkgJSON string) string {
	raw, err := os.ReadFile(pkgJSON)
	if err != nil {
		return ""
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Name)
}

func readWorkspaceGlobs(projectRoot string) []string {
	var out []string
	raw, err := os.ReadFile(filepath.Join(projectRoot, "package.json"))
	if err == nil {
		var pkg struct {
			Workspaces json.RawMessage `json:"workspaces"`
		}
		if json.Unmarshal(raw, &pkg) == nil && len(pkg.Workspaces) > 0 {
			// array form
			var arr []string
			if json.Unmarshal(pkg.Workspaces, &arr) == nil {
				out = append(out, arr...)
			} else {
				var obj struct {
					Packages []string `json:"packages"`
				}
				if json.Unmarshal(pkg.Workspaces, &obj) == nil {
					out = append(out, obj.Packages...)
				}
			}
		}
	}
	// pnpm-workspace.yaml
	if yaml, err := os.ReadFile(filepath.Join(projectRoot, "pnpm-workspace.yaml")); err == nil {
		out = append(out, parsePnpmPackages(string(yaml))...)
	}
	return out
}

func parsePnpmPackages(yaml string) []string {
	var out []string
	inPackages := false
	for _, line := range strings.Split(yaml, "\n") {
		if strings.Contains(line, "packages:") && strings.TrimSpace(strings.Split(line, ":")[0]) == "packages" {
			inPackages = true
			continue
		}
		if !inPackages {
			continue
		}
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "-") {
			item := strings.TrimSpace(strings.TrimPrefix(trim, "-"))
			item = strings.Trim(item, `"'`)
			if item != "" {
				out = append(out, item)
			}
			continue
		}
		// non-indented key ends the block
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			inPackages = false
		}
	}
	return out
}

func expandWorkspaceGlob(projectRoot, pattern string) []string {
	norm := filepath.ToSlash(strings.TrimRight(pattern, "/"))
	if strings.Contains(norm, "**") {
		return expandDoubleStarGlob(projectRoot, norm)
	}
	// filepath.Glob handles multi-level segment patterns like apps/*/lib that
	// a single ReadDir layer cannot express.
	matches, err := filepath.Glob(filepath.Join(projectRoot, filepath.FromSlash(norm)))
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || !st.IsDir() {
			continue
		}
		name := filepath.Base(m)
		if name == "node_modules" || strings.HasPrefix(name, ".") {
			continue
		}
		rel, err := filepath.Rel(projectRoot, m)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

// expandDoubleStarGlob walks the directory tree under the pattern prefix for
// "**" globs (packages/**, apps/**/lib style). The prefix before the first
// "**" is walked recursively; the segments after it are matched per path
// segment, where "**" matches zero or more whole segments (npm/yarn
// semantics: apps/**/lib matches apps/web/lib and apps/a/b/lib). node_modules
// and hidden directories are never descended into. A trailing "**"
// (packages/**) matches every directory below the prefix.
func expandDoubleStarGlob(projectRoot, pattern string) []string {
	if !strings.Contains(pattern, "**") {
		return nil
	}
	segs := strings.Split(pattern, "/")
	starIdx := -1
	var tail []string
	for i, s := range segs {
		if starIdx < 0 && strings.Contains(s, "**") {
			starIdx = i
		}
		if starIdx < 0 {
			continue
		}
		if s == "**" {
			// consecutive "**" segments collapse into one wildcard
			if len(tail) == 0 || tail[len(tail)-1] != "**" {
				tail = append(tail, "**")
			}
			continue
		}
		tail = append(tail, s)
	}
	if starIdx < 0 {
		return nil
	}
	if len(tail) == 0 {
		// pattern ended right after "**" (packages/**): match any depth
		tail = []string{"**"}
	}
	prefix := strings.Join(segs[:starIdx], "/")
	base := filepath.Join(projectRoot, filepath.FromSlash(prefix))
	var out []string
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path == base {
			return nil // the prefix itself is the container, not a package
		}
		// Never descend into dependencies, VCS or hidden dirs: their nested
		// package.json files would pollute the workspace table and silently
		// misroute imports (must-fix: returning nil instead of fs.SkipDir
		// walked straight into node_modules/.git).
		name := filepath.Base(path)
		if name == "node_modules" || strings.HasPrefix(name, ".") {
			return fs.SkipDir
		}
		rel, rerr := filepath.Rel(projectRoot, path)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		rest := strings.TrimPrefix(relSlash, prefix)
		rest = strings.TrimPrefix(rest, "/")
		if !matchSegments(tail, strings.Split(rest, "/")) {
			return nil
		}
		out = append(out, relSlash)
		return nil
	})
	sort.Strings(out)
	return out
}

// matchSegments reports whether the glob tail segments (which may contain
// "**", matching zero or more whole segments) match the path segments rest.
// Non-wildcard segments are matched with filepath.Match, so '*'/'?' keep
// working within a single segment (they never cross '/').
func matchSegments(tail, rest []string) bool {
	for len(tail) > 0 {
		if tail[0] == "**" {
			for i := 0; i <= len(rest); i++ {
				if matchSegments(tail[1:], rest[i:]) {
					return true
				}
			}
			return false
		}
		if len(rest) == 0 {
			return false
		}
		ok, err := filepath.Match(tail[0], rest[0])
		if err != nil || !ok {
			return false
		}
		tail, rest = tail[1:], rest[1:]
	}
	return len(rest) == 0
}

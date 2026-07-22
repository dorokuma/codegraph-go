package resolution

import (
	"encoding/json"
	"os"
	"path/filepath"
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
			pkgName := readPackageName(filepath.Join(projectRoot, dir))
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
	star := strings.IndexByte(norm, '*')
	if star < 0 {
		return []string{norm}
	}
	base := strings.TrimRight(norm[:star], "/")
	entries, err := os.ReadDir(filepath.Join(projectRoot, base))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
			continue
		}
		if base == "" {
			out = append(out, e.Name())
		} else {
			out = append(out, base+"/"+e.Name())
		}
	}
	return out
}

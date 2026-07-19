package resolution

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// CargoWorkspace maps crate names (and hyphen→underscore aliases) to member dirs
// relative to the workspace root (posix slash). Official cargo-workspace.ts.
type CargoWorkspace struct {
	RootDir string
	// ByName: crate name / underscored alias → member dir relative to root ("." for root package)
	ByName map[string]string
}

var (
	cargoCacheMu sync.Mutex
	cargoCache   = map[string]*cargoCacheEntry{}
)

type cargoCacheEntry struct {
	modTime int64
	ws      *CargoWorkspace
}

// ClearCargoCache drops cached Cargo.toml parses.
func ClearCargoCache() {
	cargoCacheMu.Lock()
	defer cargoCacheMu.Unlock()
	cargoCache = map[string]*cargoCacheEntry{}
}

var (
	tomlNameRe   = regexp.MustCompile(`(?m)^\s*name\s*=\s*["']([^"'\n]+)["']`)
	globCharsRe  = regexp.MustCompile(`[*?\[\]{}!]`)
	cargoSkipDir = map[string]bool{
		"target": true, "node_modules": true, ".git": true,
		"dist": true, "build": true, ".codegraph": true,
	}
)

// LoadCargoWorkspace reads root Cargo.toml (+ member manifests).
// Returns nil when no Cargo.toml or nothing usable.
func LoadCargoWorkspace(projectRoot string) *CargoWorkspace {
	projectRoot = filepath.Clean(projectRoot)
	rootToml := filepath.Join(projectRoot, "Cargo.toml")
	st, err := os.Stat(rootToml)
	if err != nil {
		return nil
	}
	mtime := st.ModTime().UnixNano()
	cargoCacheMu.Lock()
	if e, ok := cargoCache[projectRoot]; ok && e.modTime == mtime {
		w := e.ws
		cargoCacheMu.Unlock()
		return w
	}
	cargoCacheMu.Unlock()

	raw, err := os.ReadFile(rootToml)
	if err != nil {
		return nil
	}
	content := string(raw)
	byName := map[string]string{}

	// Root package (single-crate or workspace virtual root with [package])
	if name := parseTomlPackageName(content); name != "" {
		addCrateAlias(byName, name, ".")
	}

	// Workspace members
	for _, member := range expandCargoMembers(projectRoot, parseWorkspaceMembers(content)) {
		memberToml := filepath.Join(projectRoot, filepath.FromSlash(member), "Cargo.toml")
		mraw, err := os.ReadFile(memberToml)
		if err != nil {
			continue
		}
		pkg := parseTomlPackageName(string(mraw))
		if pkg == "" {
			continue
		}
		addCrateAlias(byName, pkg, member)
	}

	var ws *CargoWorkspace
	if len(byName) > 0 {
		ws = &CargoWorkspace{RootDir: projectRoot, ByName: byName}
	}
	cargoCacheMu.Lock()
	cargoCache[projectRoot] = &cargoCacheEntry{modTime: mtime, ws: ws}
	cargoCacheMu.Unlock()
	return ws
}

func addCrateAlias(m map[string]string, crateName, memberPath string) {
	memberPath = strings.Trim(filepath.ToSlash(memberPath), "/")
	if memberPath == "" {
		memberPath = "."
	}
	// First wins
	if _, ok := m[crateName]; !ok {
		m[crateName] = memberPath
	}
	norm := strings.ReplaceAll(crateName, "-", "_")
	if norm != crateName {
		if _, ok := m[norm]; !ok {
			m[norm] = memberPath
		}
	}
}

func parseTomlPackageName(content string) string {
	sec := getTomlSection(content, "package")
	if sec == "" {
		return ""
	}
	m := tomlNameRe.FindStringSubmatch(sec)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func parseWorkspaceMembers(content string) []string {
	sec := getTomlSection(content, "workspace")
	if sec == "" {
		return nil
	}
	arr := getTomlArrayValue(sec, "members")
	if arr == "" {
		return nil
	}
	return extractQuotedValues(arr)
}

// getTomlSection returns body lines of [name] (not [name.foo]).
func getTomlSection(content, name string) string {
	header := "[" + name + "]"
	lines := strings.Split(content, "\n")
	var body []string
	in := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		// strip comments
		if i := strings.Index(trim, "#"); i >= 0 {
			trim = strings.TrimSpace(trim[:i])
		}
		if !in {
			if trim == header {
				in = true
			}
			continue
		}
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			break
		}
		body = append(body, line)
	}
	if !in {
		return ""
	}
	return strings.Join(body, "\n")
}

func getTomlArrayValue(section, key string) string {
	// Find key =
	re := regexp.MustCompile(`(?m)\b` + regexp.QuoteMeta(key) + `\b\s*=`)
	loc := re.FindStringIndex(section)
	if loc == nil {
		return ""
	}
	i := loc[1]
	for i < len(section) && (section[i] == ' ' || section[i] == '\t' || section[i] == '\n' || section[i] == '\r') {
		i++
	}
	if i >= len(section) || section[i] != '[' {
		return ""
	}
	i++ // past [
	start := i
	depth := 1
	inQuote := byte(0)
	esc := false
	for i < len(section) {
		ch := section[i]
		if inQuote != 0 {
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
			} else if ch == inQuote {
				inQuote = 0
			}
			i++
			continue
		}
		if ch == '"' || ch == '\'' {
			inQuote = ch
			i++
			continue
		}
		if ch == '[' {
			depth++
		} else if ch == ']' {
			depth--
			if depth == 0 {
				return section[start:i]
			}
		}
		i++
	}
	return ""
}

func extractQuotedValues(s string) []string {
	var out []string
	var quote byte
	var cur strings.Builder
	esc := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote == 0 {
			if ch == '"' || ch == '\'' {
				quote = ch
				cur.Reset()
			}
			continue
		}
		if esc {
			cur.WriteByte(ch)
			esc = false
			continue
		}
		if ch == '\\' {
			esc = true
			continue
		}
		if ch == quote {
			v := strings.TrimSpace(cur.String())
			if v != "" {
				out = append(out, v)
			}
			quote = 0
			continue
		}
		cur.WriteByte(ch)
	}
	return out
}

func expandCargoMembers(projectRoot string, members []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range members {
		m = filepath.ToSlash(strings.Trim(m, "/"))
		var cands []string
		if globCharsRe.MatchString(m) {
			cands = expandCargoGlob(projectRoot, m)
		} else {
			cands = []string{m}
		}
		for _, c := range cands {
			c = filepath.ToSlash(strings.Trim(c, "/"))
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func expandCargoGlob(projectRoot, pattern string) []string {
	// Simple cases: "crates/*", "crates/*/foo", exact prefix before first glob char.
	first := globCharsRe.FindStringIndex(pattern)
	if first == nil {
		return []string{pattern}
	}
	prefix := pattern[:first[0]]
	// drop partial segment after last /
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		prefix = prefix[:i]
	} else {
		prefix = ""
	}
	var matches []string
	var walk func(rel string, depth int)
	walk = func(rel string, depth int) {
		if depth > 5 {
			return
		}
		dir := projectRoot
		if rel != "" && rel != "." {
			dir = filepath.Join(projectRoot, filepath.FromSlash(rel))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if cargoSkipDir[name] || strings.HasPrefix(name, ".") {
				continue
			}
			child := name
			if rel != "" && rel != "." {
				child = rel + "/" + name
			}
			if cargoGlobMatch(pattern, child) {
				matches = append(matches, child)
			}
			walk(child, depth+1)
		}
	}
	start := prefix
	if start == "" {
		start = "."
	}
	walk(start, 0)
	return matches
}

// cargoGlobMatch: very small glob — * matches one path segment, ** not needed for cargo members.
func cargoGlobMatch(pattern, path string) bool {
	pp := strings.Split(filepath.ToSlash(pattern), "/")
	ss := strings.Split(filepath.ToSlash(path), "/")
	return cargoGlobSegs(pp, ss)
}

func cargoGlobSegs(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		// not used often; treat as *
		pat[0] = "*"
	}
	if len(path) == 0 {
		return false
	}
	if pat[0] == "*" {
		return cargoGlobSegs(pat[1:], path[1:])
	}
	if matched, _ := filepath.Match(pat[0], path[0]); matched {
		return cargoGlobSegs(pat[1:], path[1:])
	}
	return false
}

// ResolveCargoImport maps a Rust use/path prefix to candidate source files.
// spec examples: "mytool_core", "mytool_core::util", "crate::util" (needs fromFile).
func ResolveCargoImport(workdir, fromFile, spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	// Strip trailing ::*
	spec = strings.TrimSuffix(spec, "::*")
	spec = strings.TrimSuffix(spec, "::self")
	parts := strings.Split(spec, "::")
	if len(parts) == 0 || parts[0] == "" {
		return nil
	}

	workdir = filepath.Clean(workdir)
	ws := LoadCargoWorkspace(workdir)
	// Also try nearest Cargo.toml above fromFile when workdir is broad.
	if ws == nil && fromFile != "" {
		if near := findNearestCargoRoot(fromFile, workdir); near != "" {
			ws = LoadCargoWorkspace(near)
			if ws != nil {
				workdir = near
			}
		}
	}

	first := parts[0]
	rest := parts[1:]

	// crate:: / self:: / super:: — relative to current crate
	if first == "crate" || first == "self" || first == "super" {
		crateRoot := findRustCrateSrcRoot(fromFile, workdir)
		if crateRoot == "" {
			return nil
		}
		return resolveRustModuleUnder(crateRoot, fromFile, first, rest)
	}

	// External/workspace crate name
	if ws != nil {
		if member, ok := ws.ByName[first]; ok {
			memberAbs := workdir
			if member != "." {
				memberAbs = filepath.Join(workdir, filepath.FromSlash(member))
			}
			srcRoot := filepath.Join(memberAbs, "src")
			if len(rest) == 0 {
				// bare crate → lib.rs / main.rs
				return rustCrateEntryFiles(srcRoot)
			}
			// Full path may end with a symbol (use crate::func), not a module file.
			if hit := resolveRustModuleUnder(srcRoot, "", "crate", rest); len(hit) > 0 {
				return hit
			}
			if len(rest) > 1 {
				if hit := resolveRustModuleUnder(srcRoot, "", "crate", rest[:len(rest)-1]); len(hit) > 0 {
					return hit
				}
			}
			// Symbol imported from crate root
			return rustCrateEntryFiles(srcRoot)
		}
	}

	// Fallback: treat as path under src/ of nearest crate
	if crateRoot := findRustCrateSrcRoot(fromFile, workdir); crateRoot != "" {
		// first segment as submodule of current crate (2018 edition self-relative)
		if hit := resolveRustModuleUnder(crateRoot, fromFile, "self", parts); len(hit) > 0 {
			return hit
		}
		if len(parts) > 1 {
			if hit := resolveRustModuleUnder(crateRoot, fromFile, "self", parts[:len(parts)-1]); len(hit) > 0 {
				return hit
			}
		}
		return rustCrateEntryFiles(crateRoot)
	}
	return nil
}

func rustCrateEntryFiles(srcRoot string) []string {
	var out []string
	for _, name := range []string{"lib.rs", "main.rs"} {
		p := filepath.Join(srcRoot, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, p)
		}
	}
	return uniqueExisting(out)
}

// findRustCrateSrcRoot walks up from fromFile for a dir containing lib.rs or main.rs
// (the crate's src/ directory), or Cargo.toml then src/.
func findRustCrateSrcRoot(fromFile, stopAt string) string {
	if fromFile == "" {
		// try stopAt/src
		src := filepath.Join(stopAt, "src")
		if hasRustEntry(src) {
			return src
		}
		return ""
	}
	cur := filepath.Dir(fromFile)
	stopAt = filepath.Clean(stopAt)
	for i := 0; i < 64; i++ {
		// already in src/ with entry files
		if hasRustEntry(cur) {
			return cur
		}
		// Cargo.toml sibling → src/
		if _, err := os.Stat(filepath.Join(cur, "Cargo.toml")); err == nil {
			src := filepath.Join(cur, "src")
			if hasRustEntry(src) {
				return src
			}
		}
		if cur == stopAt {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	src := filepath.Join(stopAt, "src")
	if hasRustEntry(src) {
		return src
	}
	return ""
}

func hasRustEntry(dir string) bool {
	for _, name := range []string{"lib.rs", "main.rs"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

func findNearestCargoRoot(fromFile, stopAt string) string {
	cur := filepath.Dir(fromFile)
	stopAt = filepath.Clean(stopAt)
	for {
		if _, err := os.Stat(filepath.Join(cur, "Cargo.toml")); err == nil {
			return cur
		}
		if cur == stopAt {
			if _, err := os.Stat(filepath.Join(stopAt, "Cargo.toml")); err == nil {
				return stopAt
			}
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// resolveRustModuleUnder walks module segments under src root.
// anchor: crate | self | super (super walks up from current module dir).
func resolveRustModuleUnder(srcRoot, fromFile, anchor string, segs []string) []string {
	var start string
	switch anchor {
	case "crate":
		start = srcRoot
	case "self":
		start = rustSelfModuleDir(fromFile, srcRoot)
	case "super":
		start = rustSelfModuleDir(fromFile, srcRoot)
		// count leading supers already stripped? segs may still contain super
		for len(segs) > 0 && segs[0] == "super" {
			start = filepath.Dir(start)
			segs = segs[1:]
		}
		start = filepath.Dir(start) // one super for the anchor itself
	default:
		start = srcRoot
	}
	if start == "" {
		return nil
	}

	// strip remaining self/crate noise
	var clean []string
	for _, s := range segs {
		if s == "" || s == "self" || s == "crate" {
			continue
		}
		if s == "super" {
			start = filepath.Dir(start)
			continue
		}
		clean = append(clean, s)
	}
	if len(clean) == 0 {
		return rustCrateEntryFiles(srcRoot)
	}

	dir := start
	var target string
	for _, seg := range clean {
		asFile := filepath.Join(dir, seg+".rs")
		asMod := filepath.Join(dir, seg, "mod.rs")
		if st, err := os.Stat(asFile); err == nil && !st.IsDir() {
			target = asFile
			dir = filepath.Join(dir, seg)
			continue
		}
		if st, err := os.Stat(asMod); err == nil && !st.IsDir() {
			target = asMod
			dir = filepath.Join(dir, seg)
			continue
		}
		return nil
	}
	if target != "" {
		return []string{target}
	}
	return nil
}

func rustSelfModuleDir(fromFile, srcRoot string) string {
	if fromFile == "" {
		return srcRoot
	}
	base := filepath.Base(fromFile)
	dir := filepath.Dir(fromFile)
	if base == "mod.rs" || base == "lib.rs" || base == "main.rs" {
		return dir
	}
	// foo.rs → foo/
	return filepath.Join(dir, strings.TrimSuffix(base, ".rs"))
}

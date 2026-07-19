package resolution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// AliasPattern is one compilerOptions.paths entry (official path-aliases.ts).
type AliasPattern struct {
	Prefix       string
	Suffix       string
	HasWildcard  bool
	Replacements []string // relative to baseUrl; may contain *
}

// AliasMap is the project-level tsconfig/jsconfig paths map.
type AliasMap struct {
	BaseURL  string // absolute
	Patterns []AliasPattern
}

var (
	aliasCacheMu sync.Mutex
	aliasCache   = map[string]*aliasCacheEntry{}
)

type aliasCacheEntry struct {
	modTime int64
	aliases *AliasMap // nil means "loaded, no aliases"
}

// ClearAliasCache drops cached tsconfig/jsconfig parses (tests / reindex).
func ClearAliasCache() {
	aliasCacheMu.Lock()
	defer aliasCacheMu.Unlock()
	aliasCache = map[string]*aliasCacheEntry{}
}

// LoadProjectAliases reads tsconfig.json then jsconfig.json under projectRoot.
// Returns nil when no usable paths block exists.
func LoadProjectAliases(projectRoot string) *AliasMap {
	projectRoot = filepath.Clean(projectRoot)
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		p := filepath.Join(projectRoot, name)
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		mtime := st.ModTime().UnixNano()
		aliasCacheMu.Lock()
		if e, ok := aliasCache[projectRoot]; ok && e.modTime == mtime {
			a := e.aliases
			aliasCacheMu.Unlock()
			return a
		}
		aliasCacheMu.Unlock()

		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		aliases := parseTsconfigAliases(projectRoot, raw)
		aliasCacheMu.Lock()
		aliasCache[projectRoot] = &aliasCacheEntry{modTime: mtime, aliases: aliases}
		// Only cache under projectRoot once we found a parseable file.
		aliasCacheMu.Unlock()
		return aliases
	}
	aliasCacheMu.Lock()
	aliasCache[projectRoot] = &aliasCacheEntry{modTime: 0, aliases: nil}
	aliasCacheMu.Unlock()
	return nil
}

type rawTsconfig struct {
	CompilerOptions *struct {
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

func parseTsconfigAliases(projectRoot string, raw []byte) *AliasMap {
	cleaned := stripJSONC(string(raw))
	var cfg rawTsconfig
	if err := json.Unmarshal([]byte(cleaned), &cfg); err != nil {
		return nil
	}
	if cfg.CompilerOptions == nil || len(cfg.CompilerOptions.Paths) == 0 {
		return nil
	}
	baseRel := cfg.CompilerOptions.BaseURL
	if baseRel == "" {
		baseRel = "."
	}
	baseURL := filepath.Clean(filepath.Join(projectRoot, baseRel))

	var patterns []AliasPattern
	for pattern, targets := range cfg.CompilerOptions.Paths {
		if len(targets) == 0 {
			continue
		}
		var filtered []string
		for _, t := range targets {
			if strings.TrimSpace(t) != "" {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) == 0 {
			continue
		}
		prefix, suffix, hasStar := splitWildcard(pattern)
		patterns = append(patterns, AliasPattern{
			Prefix:       prefix,
			Suffix:       suffix,
			HasWildcard:  hasStar,
			Replacements: filtered,
		})
	}
	if len(patterns) == 0 {
		return nil
	}
	// Longer prefix first; literal before wildcard at same length.
	sort.SliceStable(patterns, func(i, j int) bool {
		if len(patterns[i].Prefix) != len(patterns[j].Prefix) {
			return len(patterns[i].Prefix) > len(patterns[j].Prefix)
		}
		if patterns[i].HasWildcard != patterns[j].HasWildcard {
			return !patterns[i].HasWildcard
		}
		return false
	})
	return &AliasMap{BaseURL: baseURL, Patterns: patterns}
}

func splitWildcard(pattern string) (prefix, suffix string, has bool) {
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return pattern, "", false
	}
	return pattern[:star], pattern[star+1:], true
}

// ApplyAliases rewrites an import specifier through AliasMap.
// Returns project-root-relative slash paths (no extension), or nil.
func ApplyAliases(importPath string, aliases *AliasMap, projectRoot string) []string {
	if aliases == nil || importPath == "" {
		return nil
	}
	projectRoot = filepath.Clean(projectRoot)
	for _, pat := range aliases.Patterns {
		if !strings.HasPrefix(importPath, pat.Prefix) {
			continue
		}
		if pat.Suffix != "" && !strings.HasSuffix(importPath, pat.Suffix) {
			continue
		}
		var captured string
		if pat.HasWildcard {
			end := len(importPath) - len(pat.Suffix)
			if end < len(pat.Prefix) {
				continue
			}
			captured = importPath[len(pat.Prefix):end]
		} else if importPath != pat.Prefix {
			continue
		}
		var out []string
		for _, target := range pat.Replacements {
			filled := target
			if pat.HasWildcard {
				filled = strings.Replace(target, "*", captured, 1)
			}
			abs := filepath.Clean(filepath.Join(aliases.BaseURL, filled))
			rel, err := filepath.Rel(projectRoot, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return out
	}
	return nil
}

// Conventional fallback aliases when no tsconfig paths match (official list).
var fallbackAliases = []struct{ prefix, repl string }{
	{"@/", "src/"},
	{"~/", "src/"},
	{"@src/", "src/"},
	{"src/", "src/"},
	{"@app/", "app/"},
	{"app/", "app/"},
}

// ApplyFallbackAliases maps @/foo → src/foo style conventions.
func ApplyFallbackAliases(importPath string) string {
	for _, a := range fallbackAliases {
		if strings.HasPrefix(importPath, a.prefix) {
			return a.repl + strings.TrimPrefix(importPath, a.prefix)
		}
	}
	return ""
}

// stripJSONC removes // and /* */ comments and trailing commas outside strings.
func stripJSONC(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	inString := false
	i := 0
	for i < len(src) {
		ch := src[i]
		if inString {
			out.WriteByte(ch)
			if ch == '\\' && i+1 < len(src) {
				out.WriteByte(src[i+1])
				i += 2
				continue
			}
			if ch == '"' {
				inString = false
			}
			i++
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			i++
			continue
		}
		if ch == '/' && i+1 < len(src) && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < len(src) && src[i+1] == '*' {
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		out.WriteByte(ch)
		i++
	}
	// trailing commas before } or ]
	re := regexp.MustCompile(`,(\s*[}\]])`)
	return re.ReplaceAllString(out.String(), "$1")
}

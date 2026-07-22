package db

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxBodyChars caps stored symbol bodies so the DB and FTS stay lean.
// Full source is still available via the context tool / file read.
const MaxBodyChars = 8_000

// TruncateBody shortens oversized bodies on a rune-safe boundary.
func TruncateBody(body string) string {
	if len(body) <= MaxBodyChars {
		return body
	}
	// Prefer cutting at a newline near the limit.
	cut := MaxBodyChars
	if i := strings.LastIndex(body[:cut], "\n"); i > MaxBodyChars*3/4 {
		cut = i
	}
	// Ensure we don't split a multi-byte rune.
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + "\n/* ... body truncated ... */"
}

// callTargetKinds are symbol kinds that make sense as call targets.
var callTargetKinds = map[string]bool{
	KindFunction:  true,
	KindMethod:    true,
	"route":       true,
	KindClass:     true, // constructors / type refs
	KindStruct:    true,
	KindInterface: true,
	"foreign_function": true,
}

// ResolveBestTarget picks the best definition for a call/import name using
// proximity scoring (official CodeGraph name-matcher idea, simplified):
//
//	same file > same directory > same parent dir > first match
//
// Ambiguous ubiquitous names (too many candidates) return 0 rather than a
// low-confidence wrong edge — better no edge than a wrong one.
func ResolveBestTarget(candidates []Node, fromFile string, preferCallTarget bool) int64 {
	if len(candidates) == 0 {
		return 0
	}

	const ambiguousCeiling = 80
	if len(candidates) > ambiguousCeiling {
		// Still allow exact same-file hits.
		for _, c := range candidates {
			if c.File == fromFile && (!preferCallTarget || callTargetKinds[c.Kind]) {
				return c.ID
			}
		}
		return 0
	}

	bestID := int64(0)
	bestScore := -1
	fromDir := filepath.Dir(fromFile)
	fromParent := filepath.Dir(fromDir)

	for _, c := range candidates {
		if preferCallTarget && !callTargetKinds[c.Kind] {
			// file/module placeholders are weak call targets
			if c.Kind == KindFile || c.Kind == "module" {
				continue
			}
		}
		score := 0
		if c.File == fromFile {
			score += 100
		} else if filepath.Dir(c.File) == fromDir {
			score += 50
		} else if filepath.Dir(filepath.Dir(c.File)) == fromParent || filepath.Dir(c.File) == fromParent {
			score += 20
		}
		// Prefer real functions/methods over classes for call edges
		switch c.Kind {
		case KindFunction, KindMethod:
			score += 5
		case "route":
			score += 3
		}
		// Prefer definitions that have a body
		if c.Body != "" {
			score += 1
		}
		if score > bestScore {
			bestScore = score
			bestID = c.ID
		}
	}
	// Require at least some signal if multiple candidates (avoid pure first-hit).
	// bestScore starts at -1 and every candidate with score>=0 sets bestID≠0,
	// so bestID is always non-zero here. Threshold check must be on bestScore.
	if bestScore < 5 {
		return 0
	}
	return bestID
}

// RelPath makes paths shorter for agent-facing output.
func RelPath(workdir, file string) string {
	if workdir == "" {
		return file
	}
	if rel, err := filepath.Rel(workdir, file); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return file
}

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

// callTargetKinds are symbol kinds that make sense as call targets. Kept in
// sync with internal/resolution/name_matcher.go (the resolution package is
// the production path; this copy serves the deprecated ResolveBestTarget and
// must not drift from it): structs and interfaces are excluded — a call edge
// to a struct/interface node is almost always a wrong match (audit: calls
// linked to class/struct nodes). Classes stay (Python/JS instantiation is a
// real call pattern); route and foreign_function are framework targets.
// Signature nodes are interface-method declarations, never call targets, so
// they are excluded too (they fall out via the map, like file/module).
var callTargetKinds = map[string]bool{
	KindFunction:       true,
	KindMethod:         true,
	"route":            true,
	KindClass:          true,
	"foreign_function": true,
}

// ResolveBestTarget picks the best definition for a call/import name using
// proximity scoring (official CodeGraph name-matcher idea, simplified):
//
//	same file > same directory > same parent dir > first match
//
// Ambiguous ubiquitous names (too many candidates) return 0 rather than a
// low-confidence wrong edge — better no edge than a wrong one.
//
// Deprecated: superseded by internal/resolution.MatchName, which the
// resolution pass uses for all call/import edges. Kept for legacy callers
// and tests; its callTargetKinds must stay aligned with name_matcher.go
// (both now exclude struct/interface/signature/file/module from call
// targets).
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
			// Call edges must not attach to non-callable kinds (structs,
			// interfaces, files, modules, signatures, variables, …) even
			// when no function matches. Same rule as MatchName.
			continue
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

// StoragePath returns the workdir-relative, slash-normalized path used as the
// index key for files/nodes/edges. Storing relative paths keeps the DB portable
// across machines and checkouts. Paths outside workdir fall back to a cleaned
// absolute form so callers still have a stable key.
func StoragePath(workdir, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if workdir == "" {
		return filepath.ToSlash(filepath.Clean(path))
	}
	wd := filepath.Clean(workdir)
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(wd, target)
	}
	target = filepath.Clean(target)
	rel, err := filepath.Rel(wd, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(target)
	}
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

// AbsPath joins a storage key (relative or absolute) under workdir for disk I/O.
//
// Relative keys are jailed inside workdir: a key that would escape (e.g. a
// hand-crafted or malicious "../../etc/passwd" row in the index) returns ""
// instead of a path outside the workspace, so callers doing disk reads through
// AbsPath cannot be tricked into reading arbitrary files. Absolute stored keys
// are returned as-is (legacy indexes store out-of-workdir files in absolute
// form on purpose).
func AbsPath(workdir, stored string) string {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return workdir
	}
	if filepath.IsAbs(stored) {
		return filepath.Clean(stored)
	}
	if workdir == "" {
		return filepath.Clean(stored)
	}
	wd := filepath.Clean(workdir)
	if stored == "." {
		return wd
	}
	joined := filepath.Clean(filepath.Join(wd, filepath.FromSlash(stored)))
	rel, err := filepath.Rel(wd, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Path escapes workdir — refuse rather than expose out-of-tree files.
		return ""
	}
	return joined
}

// RelPath makes paths shorter for agent-facing output.
// Handles both absolute indexed paths (legacy) and workdir-relative keys.
func RelPath(workdir, file string) string {
	if file == "" {
		return file
	}
	if workdir == "" {
		return filepath.ToSlash(file)
	}
	// Already a storage-relative key.
	if !filepath.IsAbs(file) {
		return filepath.ToSlash(filepath.Clean(file))
	}
	if rel, err := filepath.Rel(workdir, file); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return file
}

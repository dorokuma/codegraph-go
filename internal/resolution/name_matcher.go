package resolution

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// Provenance labels written onto resolved edges.
const (
	ProvExact     = "exact"
	ProvImport    = "import"
	ProvProximity = "proximity"
	ProvHeuristic = "heuristic"
)

// ambiguousCeiling: above this, refuse fuzzy global pick (same idea as official).
const ambiguousCeiling = 80

// callTargetKinds are symbol kinds that make sense as call targets. Structs
// and interfaces are excluded: a call edge to a struct/interface node is
// almost always a wrong match (audit: calls linked to class/struct nodes).
// Classes stay (Python/JS instantiation is a real call pattern); route and
// foreign_function are framework targets.
var callTargetKinds = map[string]bool{
	db.KindFunction:    true,
	db.KindMethod:      true,
	"route":            true,
	db.KindClass:       true,
	"foreign_function": true,
}

// CallTargetKinds returns a read-only snapshot of callTargetKinds. Exported
// solely so the cross-package consistency test can deep-compare this copy
// against internal/db's snapshot; production code keeps using the
// package-local map.
func CallTargetKinds() map[string]bool {
	out := make(map[string]bool, len(callTargetKinds))
	for kind, want := range callTargetKinds {
		out[kind] = want
	}
	return out
}

// MatchResult is a name-matcher hit.
type MatchResult struct {
	TargetID   int64
	Provenance string
	Score      int
}

// MatchName picks the best definition for a reference name.
// Priority: same file > same directory > same parent > unique global.
// Supports tail segment (util.greet → greet) and Class.method form.
func MatchName(candidates []db.Node, refName, fromFile string, preferCall bool) MatchResult {
	if len(candidates) == 0 {
		return MatchResult{}
	}

	// Prefer exact name; if none, try tail segment filtering is done by caller
	// via candidate collection. Here we only score the given set.

	if len(candidates) > ambiguousCeiling {
		// Only allow exact same-file hits when ubiquitous.
		for _, c := range candidates {
			if c.File == fromFile && (!preferCall || callTargetKinds[c.Kind]) {
				return MatchResult{TargetID: c.ID, Provenance: ProvExact, Score: 100}
			}
		}
		return MatchResult{}
	}

	best := MatchResult{}
	bestCount := 0
	fromDir := filepath.Dir(fromFile)
	fromParent := filepath.Dir(fromDir)

	for _, c := range candidates {
		if preferCall && !callTargetKinds[c.Kind] {
			// Call edges must not attach to non-callable kinds (structs,
			// interfaces, files, modules, …) even when no function matches.
			continue
		}
		score := 0
		prov := ProvHeuristic
		if c.File == fromFile {
			score += 100
			prov = ProvExact
		} else if filepath.Dir(c.File) == fromDir {
			score += 50
			prov = ProvProximity
		} else if filepath.Dir(c.File) == fromParent || filepath.Dir(filepath.Dir(c.File)) == fromParent {
			score += 20
			prov = ProvProximity
		}
		switch c.Kind {
		case db.KindFunction, db.KindMethod:
			score += 5
		case "route":
			score += 3
		}
		if c.Body != "" {
			score++
		}
		// Prefer exact name over tail-only (qualified match signal).
		if c.Name == refName {
			score += 2
		}
		if score > best.Score {
			best = MatchResult{TargetID: c.ID, Provenance: prov, Score: score}
			bestCount = 1
		} else if score == best.Score && score > 0 {
			// Same top score from a different candidate: ambiguous. DB order
			// is not a stable tie-breaker and first-wins produced wrong edges
			// (audit: two same-named symbols in the same file/dir).
			bestCount++
		}
	}

	if bestCount > 1 {
		return MatchResult{} // ambiguous — refuse to guess
	}
	if len(candidates) > 1 && best.Score < 5 && preferCall {
		return MatchResult{}
	}
	// Unique global with weak score still counts as heuristic.
	if best.TargetID != 0 && best.Provenance == "" {
		best.Provenance = ProvHeuristic
	}
	return best
}

// CollectCandidates gathers nodes whose name equals ref or its tail segment,
// paginating through all matches to avoid silently losing candidates.
func CollectCandidates(database *db.DB, refName string) ([]db.Node, error) {
	return collectCandidatesContext(context.Background(), database, refName)
}

func collectCandidatesContext(ctx context.Context, database *db.DB, refName string) ([]db.Node, error) {
	refName = strings.TrimSpace(refName)
	if refName == "" {
		return nil, nil
	}
	seen := map[int64]bool{}
	var out []db.Node

	add := func(n db.Node) {
		if !seen[n.ID] {
			seen[n.ID] = true
			out = append(out, n)
		}
	}

	// Resolution candidates need only ID/Kind/Name/File and the body's
	// truthiness — ForEachNodeByNameResolveContext projects exactly that
	// (body as a one-byte sentinel) instead of materializing full bodies.
	// Verified: no CollectCandidates caller consumes Body/Signature/Docstring
	// beyond MatchName's `Body != ""` truthiness, which the sentinel keeps.
	if err := database.ForEachNodeByNameResolveContext(ctx, refName, func(n db.Node) error {
		add(n)
		return nil
	}); err != nil {
		return nil, err
	}

	// Class.method / pkg.Func / util.greet
	if tail := nameTail(refName); tail != "" && tail != refName {
		if err := database.ForEachNodeByNameResolveContext(ctx, tail, func(n db.Node) error {
			add(n)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// hasCandidateContext reports whether refName has at least one candidate
// node — the same existence check collectCandidatesContext implies, but
// streaming light rows (no body/signature/docstring) and stopping at the
// first hit. The failed-retry filter asks this per failed ref; over a large
// backlog of refs whose names resolve to nothing it skips materializing
// full Node values entirely.
func hasCandidateContext(ctx context.Context, database *db.DB, refName string) (bool, error) {
	refName = strings.TrimSpace(refName)
	if refName == "" {
		return false, nil
	}
	names := []string{refName}
	if tail := nameTail(refName); tail != "" && tail != refName {
		names = append(names, tail)
	}
	for _, name := range names {
		hit := false
		err := database.ForEachNodeByNameLightContext(ctx, name, func(db.Node) bool { return true }, func(n db.Node) error {
			hit = true
			return errStop
		})
		if err != nil && !errors.Is(err, errStop) {
			return false, err
		}
		if hit {
			return true, nil
		}
	}
	return false, nil
}

// errStop aborts a ForEach* iteration early without surfacing an error to
// the caller — hasCandidateContext only needs the first match.
var errStop = errors.New("candidate scan stop")

func nameTail(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndexAny(name, ".#@"); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

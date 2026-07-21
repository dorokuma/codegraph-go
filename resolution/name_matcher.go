package resolution

import (
	"path/filepath"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
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

var callTargetKinds = map[string]bool{
	db.KindFunction: true,
	db.KindMethod:   true,
	"route":         true,
	db.KindClass:    true,
	db.KindStruct:   true,
	db.KindInterface: true,
	"foreign_function": true,
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
	fromDir := filepath.Dir(fromFile)
	fromParent := filepath.Dir(fromDir)

	for _, c := range candidates {
		if preferCall && !callTargetKinds[c.Kind] {
			if c.Kind == db.KindFile || c.Kind == "module" {
				continue
			}
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
		}
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

// CollectCandidates gathers nodes whose name equals ref or its tail segment.
func CollectCandidates(database *db.DB, refName string) ([]db.Node, error) {
	refName = strings.TrimSpace(refName)
	if refName == "" {
		return nil, nil
	}
	seen := map[int64]bool{}
	var out []db.Node

	add := func(nodes []db.Node) {
		for _, n := range nodes {
			if seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			out = append(out, n)
		}
	}

	nodes, err := database.GetNodeByName(refName)
	if err != nil {
		return nil, err
	}
	add(nodes)

	// Class.method / pkg.Func / util.greet
	if tail := nameTail(refName); tail != "" && tail != refName {
		more, err := database.GetNodeByName(tail)
		if err != nil {
			return nil, err
		}
		add(more)
	}
	return out, nil
}

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

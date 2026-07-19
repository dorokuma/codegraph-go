package tools

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/dorokuma/codegraph-go/db"
)

// flowResult is the call-chain pack among symbols named in an explore query.
type flowResult struct {
	Text               string
	PathNodeIDs        map[int64]bool
	NamedNodeIDs       map[int64]bool
	UniqueNamedNodeIDs map[int64]bool
	NamedNodes         []db.Node // ordered, de-duped callables the query named
	PathNodes          []db.Node // spine order when a chain exists
}

var (
	exploreFileExt = regexp.MustCompile(`(?i)\.(?:java|kt|kts|ts|tsx|js|jsx|mjs|cjs|cs|py|go|rb|php|swift|rs|cpp|cc|cxx|c|h|hpp|scala|lua|dart|vue|svelte|astro|erl|hrl)$`)
	exploreIdent   = regexp.MustCompile(`^[A-Za-z_$][\w$]*(?:(?:::|\.)[\w$]+)*$`)
)

func emptyFlow() flowResult {
	return flowResult{
		PathNodeIDs:        map[int64]bool{},
		NamedNodeIDs:       map[int64]bool{},
		UniqueNamedNodeIDs: map[int64]bool{},
	}
}

// tokenizeExploreQuery splits a free-text explore query into symbol-like tokens.
func tokenizeExploreQuery(query string) []string {
	parts := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == '(' || r == ')' || r == '[' || r == ']'
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = exploreFileExt.ReplaceAllString(strings.TrimSpace(p), "")
		if len(p) < 3 || !exploreIdent.MatchString(p) {
			continue
		}
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
		if len(out) >= 16 {
			break
		}
	}
	return out
}

func isExploreCallable(kind string) bool {
	switch kind {
	case db.KindFunction, db.KindMethod, "component", "constructor":
		return true
	default:
		return false
	}
}

func tokenSegments(token string) []string {
	token = strings.ToLower(token)
	parts := strings.FieldsFunc(token, func(r rune) bool {
		return r == '.' || r == ':'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildFlowFromNamedSymbols finds the longest call/reference path among symbols
// the agent named in the query. Rules (official-aligned, step 5):
//   - bag-of-tokens query
//   - ≤1 consecutive unnamed bridge hop
//   - ambiguous simple names kept only when a co-named container matches
//   - chain of ≥2 nodes renders a Flow block (acceptance: two related symbols)
//   - root subtree filter (ExploreArgs.Path) applies to named seeds and spine hops
func buildFlowFromNamedSymbols(database *db.DB, workdir, root, query string) flowResult {
	empty := emptyFlow()
	tokens := tokenizeExploreQuery(query)
	if len(tokens) < 2 {
		return empty
	}
	if root == "" {
		root = workdir
	}

	segPool := map[string]bool{}
	for _, t := range tokens {
		for _, s := range tokenSegments(t) {
			segPool[s] = true
		}
	}

	named := map[int64]db.Node{}
	uniqueNamed := map[int64]bool{}
	var namedOrder []db.Node

	for _, t := range tokens {
		hits, err := findCallableSymbols(database, t)
		if err != nil || len(hits) == 0 {
			continue
		}
		// Path/root first — specificity and disambiguation are scoped to the subtree.
		var inRoot []db.Node
		for _, n := range hits {
			if inExploreRoot(n.File, workdir, root) {
				inRoot = append(inRoot, n)
			}
		}
		hits = inRoot
		if len(hits) == 0 {
			continue
		}
		specific := len(hits) <= 3
		var pick []db.Node
		if specific {
			pick = hits
		} else {
			for _, n := range hits {
				if containerMatches(n, segPool) {
					pick = append(pick, n)
				}
			}
		}
		if len(pick) > 6 {
			pick = pick[:6]
		}
		for _, n := range pick {
			if _, ok := named[n.ID]; ok {
				continue
			}
			named[n.ID] = n
			namedOrder = append(namedOrder, n)
			if specific {
				uniqueNamed[n.ID] = true
			}
		}
		if len(named) > 40 {
			break
		}
	}
	if len(named) < 2 {
		return empty
	}

	const maxHops = 7
	const maxBridge = 1 // ≤1 consecutive UNNAMED hop

	type parentInfo struct {
		prev     int64 // 0 = none
		edgeKind string
		node     db.Node
	}

	var best []struct {
		node     db.Node
		edgeKind string
	}

	seeds := namedOrder
	if len(seeds) > 8 {
		seeds = seeds[:8]
	}

	for _, seed := range seeds {
		parent := map[int64]parentInfo{
			seed.ID: {prev: 0, edgeKind: "", node: seed},
		}
		type qItem struct {
			id            int64
			depth, streak int
		}
		qq := []qItem{{id: seed.ID, depth: 0, streak: 0}}

		var deepID int64
		deepDepth := 0

		for h := 0; h < len(qq) && len(parent) < 1500; h++ {
			cur := qq[h]
			if cur.id != seed.ID {
				if _, isNamed := named[cur.id]; isNamed && cur.depth > deepDepth {
					deepID = cur.id
					deepDepth = cur.depth
				}
			}
			if cur.depth >= maxHops-1 {
				continue
			}
			callees, err := database.GetCalleesWithKind(cur.id)
			if err != nil {
				continue
			}
			for _, c := range callees {
				if _, seen := parent[c.ID]; seen {
					continue
				}
				// Keep the whole spine inside the path/root subtree (no cross-project bleed).
				if !inExploreRoot(c.File, workdir, root) {
					continue
				}
				// structural edges only (calls / references / bridge)
				kind := c.EdgeKind
				if kind == "" {
					kind = db.EdgeCalls
				}
				newStreak := 0
				if _, isNamed := named[c.ID]; !isNamed {
					newStreak = cur.streak + 1
				}
				if newStreak > maxBridge {
					continue
				}
				parent[c.ID] = parentInfo{prev: cur.id, edgeKind: kind, node: c.Node}
				qq = append(qq, qItem{id: c.ID, depth: cur.depth + 1, streak: newStreak})
			}
		}
		if deepID == 0 {
			continue
		}
		// reconstruct
		var chain []struct {
			node     db.Node
			edgeKind string
		}
		cur := deepID
		for cur != 0 {
			p, ok := parent[cur]
			if !ok {
				break
			}
			chain = append(chain, struct {
				node     db.Node
				edgeKind string
			}{node: p.node, edgeKind: p.edgeKind})
			cur = p.prev
		}
		// reverse
		for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
			chain[i], chain[j] = chain[j], chain[i]
		}
		if best == nil || len(chain) > len(best) {
			best = chain
		}
	}

	// Acceptance: two named symbols with a call edge → Flow (≥2 nodes).
	// Official uses ≥3; our step-5 bar is the two-symbol case.
	if len(best) < 2 {
		out := empty
		out.NamedNodeIDs = namedIDs(named)
		out.UniqueNamedNodeIDs = uniqueNamed
		out.NamedNodes = namedOrder
		return out
	}

	pathIDs := map[int64]bool{}
	pathNodes := make([]db.Node, 0, len(best))
	var b strings.Builder
	b.WriteString("**Flow (call path among the symbols you queried)**\n\n")
	for i, step := range best {
		pathIDs[step.node.ID] = true
		pathNodes = append(pathNodes, step.node)
		if i > 0 && step.edgeKind != "" {
			fmt.Fprintf(&b, "   ↓ %s\n", step.edgeKind)
		}
		rel := db.RelPath(workdir, step.node.File)
		fmt.Fprintf(&b, "%d. %s (%s:%d)\n", i+1, step.node.Name, rel, step.node.Line)
	}
	b.WriteByte('\n')
	b.WriteString("> Full source for these symbols is below — treat as already read; no need for external Read.\n\n")

	return flowResult{
		Text:               b.String(),
		PathNodeIDs:        pathIDs,
		NamedNodeIDs:       namedIDs(named),
		UniqueNamedNodeIDs: uniqueNamed,
		NamedNodes:         namedOrder,
		PathNodes:          pathNodes,
	}
}

func namedIDs(named map[int64]db.Node) map[int64]bool {
	out := make(map[int64]bool, len(named))
	for id := range named {
		out[id] = true
	}
	return out
}

func containerMatches(n db.Node, segPool map[string]bool) bool {
	qn := n.QualifiedName
	if qn == "" {
		// fall back: file base / parent dir name as weak container signal
		base := strings.TrimSuffix(filepath.Base(n.File), filepath.Ext(n.File))
		return segPool[strings.ToLower(base)]
	}
	segs := tokenSegments(qn)
	if len(segs) < 2 {
		return false
	}
	container := segs[len(segs)-2]
	return segPool[container]
}

// findCallableSymbols resolves a token to callable nodes (exact name, then tail).
func findCallableSymbols(database *db.DB, token string) ([]db.Node, error) {
	name := token
	if i := strings.LastIndexAny(token, ".#/"); i >= 0 && i+1 < len(token) {
		// keep full first; also try tail below
		name = token
	}
	nodes, err := database.GetNodeByName(name)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		// qualified tail: Class.method → method
		if i := strings.LastIndexAny(token, ".#/"); i >= 0 && i+1 < len(token) {
			nodes, err = database.GetNodeByName(token[i+1:])
			if err != nil {
				return nil, err
			}
		}
	}
	if len(nodes) == 0 {
		// last segment only for :: forms already handled; try FTS exact
		fts, err := database.FullTextSearch(lastSegment(token), 20)
		if err != nil {
			return nil, err
		}
		want := lastSegment(token)
		for _, n := range fts {
			if n.Name == want || strings.EqualFold(n.Name, want) {
				nodes = append(nodes, n)
			}
		}
	}

	var out []db.Node
	for _, n := range nodes {
		if isExploreCallable(n.Kind) {
			out = append(out, n)
		}
	}
	// Prefer exact simple-name match when token is qualified
	if strings.ContainsAny(token, ".#/") {
		want := lastSegment(token)
		var exact []db.Node
		for _, n := range out {
			if n.Name == want {
				exact = append(exact, n)
			}
		}
		if len(exact) > 0 {
			return exact, nil
		}
	}
	return out, nil
}

func lastSegment(token string) string {
	if i := strings.LastIndexAny(token, ".#/"); i >= 0 && i+1 < len(token) {
		return token[i+1:]
	}
	return token
}

// inExploreRoot reports whether file sits under root (or root is the whole workdir).
func inExploreRoot(file, workdir, root string) bool {
	if root == "" || root == workdir {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return file == root || strings.HasPrefix(file, prefix)
}

// isLowValueExploreFile reports test/spec/generated noise for tiny-tier filtering.
func isLowValueExploreFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	p := strings.ToLower(path)
	if strings.Contains(base, "_test.") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.HasSuffix(base, "_spec.go") ||
		strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "test.java") {
		return true
	}
	if strings.Contains(p, "/__tests__/") || strings.Contains(p, "/testdata/") ||
		strings.Contains(p, "/fixtures/") || strings.Contains(p, "/mocks/") {
		return true
	}
	return false
}

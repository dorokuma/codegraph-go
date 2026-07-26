package tools

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/db"

	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"
)

// CommunityArgs are the arguments for the communities tool.
type CommunityArgs struct {
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
	Max         int    `json:"max,omitempty" jsonschema:"max communities to report (default 20),optional"`
	MinSize     int    `json:"minSize,omitempty" jsonschema:"minimum community size (nodes) to include in output (default 3),optional"`
}

// CommunityResult is the result of the communities tool.
type CommunityResult struct {
	Content []ContentItem `json:"content"`
}

// provenanceWeight maps edge provenance strings to weights for the weighted
// undirected graph. Multiple edges between the same pair of nodes accumulate weight.
// Package-level so tests can inspect the mapping.
var provenanceWeight = map[string]float64{
	"exact":     1.0,
	"import":    0.8,
	"proximity": 0.3,
	"heuristic": 0.1,
}

// CommunityInfo holds one detected community for output formatting.
type CommunityInfo struct {
	ID            int               `json:"id"`
	Size          int               `json:"size"`
	InternalEdges int               `json:"internalEdges"`
	TopSymbols    []topSymbol       `json:"topSymbols"`
	TopFiles      []string          `json:"topFiles"`
	KindDist      map[string]int    `json:"kindDist"`
}

type topSymbol struct {
	Name           string  `json:"name"`
	File           string  `json:"file"`
	Line           int     `json:"line"`
	WeightedDegree float64 `json:"weightedDegree"`
}

// ToolCommunity runs community detection on the project's code graph to identify
// module/component boundaries. This helps answer global architecture questions
// like "how is this project organized?" or "what are the main modules?"
//
// The algorithm:
//  1. Fetch all nodes and edges from the index (RLock protected).
//  2. Filter out 'contains' edges (file-nesting edges that would dominate clustering).
//  3. Project directed edges as undirected with provenance-based weights.
//  4. Run Louvain community detection (resolution=1.0, fixed seed for determinism).
//  5. Report communities sorted by size descending.
func ToolCommunity(ctx context.Context, database *db.DB, workdir string, args CommunityArgs) (*CommunityResult, error) {
	if args.Max <= 0 {
		args.Max = 20
	}
	if args.MinSize <= 0 {
		args.MinSize = 3
	}

	// Step 1: Load graph snapshot
	snapshot, err := database.GetGraphSnapshot()
	if err != nil {
		return nil, fmt.Errorf("load graph snapshot: %w", err)
	}

	if len(snapshot.Nodes) == 0 {
		return &CommunityResult{
			Content: []ContentItem{{Type: "text", Text: "No nodes in index. Nothing to analyze."}},
		}, nil
	}

	// Step 2: Build node ID mapping (DB node id → 0-based compact ID for gonum)
	// and reverse mapping (compact ID → db.Node for output enrichment).
	nodeIndex := make(map[int64]int64, len(snapshot.Nodes))   // DB node ID → compact ID
	reverseIndex := make(map[int64]db.Node, len(snapshot.Nodes)) // compact ID → DB node
	g := simple.NewWeightedUndirectedGraph(0, 0)
	for i, n := range snapshot.Nodes {
		cid := int64(i)
		nodeIndex[n.ID] = cid
		reverseIndex[cid] = n
		g.AddNode(simple.Node(cid))
	}

	// Step 3: Add weighted edges.
	// Filter out 'contains' edges, project directed→undirected, accumulate weight by provenance.
	edgeWeights := make(map[[2]int64]float64)
	for _, e := range snapshot.Edges {
		if e.Kind == db.EdgeContains {
			continue
		}
		sid, sok := nodeIndex[e.SourceID]
		tid, tok := nodeIndex[e.TargetID]
		if !sok || !tok {
			continue
		}
		if sid == tid {
			continue
		}
		// Normalize direction for undirected: always use [min,max]
		if sid > tid {
			sid, tid = tid, sid
		}
		w := provenanceWeight[e.Provenance]
		if w == 0 {
			w = 0.5 // unknown provenance
		}
		edgeWeights[[2]int64{sid, tid}] += w
	}

	totalWeightedEdges := 0
	for pair, w := range edgeWeights {
		g.SetWeightedEdge(simple.WeightedEdge{
			F: simple.Node(pair[0]),
			T: simple.Node(pair[1]),
			W: w,
		})
		totalWeightedEdges++
	}

	// Step 4: Run Louvain community detection.
	// Fixed seed (42) for deterministic results.
	src := rand.NewSource(42)
	reduced := community.Modularize(g, 1.0, rand.New(src))

	// Extract communities (each element is a slice of graph.Node with compact IDs).
	rawCommunities := reduced.Communities()

	// Build a filtered list: communities >= minSize, sorted descending by size.
	type commEntry struct {
		id    int
		nodes []graph.Node
		dbIDs []int64 // DB node IDs for this community
	}
	var commList []commEntry
	for i, c := range rawCommunities {
		if len(c) < args.MinSize {
			continue
		}
		dbIDs := make([]int64, len(c))
		for j, n := range c {
			dbIDs[j] = reverseIndex[n.ID()].ID
		}
		commList = append(commList, commEntry{id: i, nodes: c, dbIDs: dbIDs})
	}
	sort.Slice(commList, func(i, j int) bool {
		return len(commList[i].nodes) > len(commList[j].nodes)
	})
	if len(commList) > args.Max {
		commList = commList[:args.Max]
	}

	// Step 5: Enrich each community with statistics.
	type enrichedComm struct {
		info CommunityInfo
	}
	infos := make([]CommunityInfo, 0, len(commList))
	for _, ce := range commList {
		info := buildCommunityInfo(ce.id, ce.nodes, ce.dbIDs, nodeIndex, reverseIndex, snapshot)
		infos = append(infos, info)
	}

	// Global modularity Q.
	q := community.Q(g, rawCommunities, 1.0)

	// Step 6: Format output.
	var b strings.Builder

	b.WriteString("# Community Detection Report\n\n")
	b.WriteString(fmt.Sprintf("Total nodes: %d\n", len(snapshot.Nodes)))
	b.WriteString(fmt.Sprintf("Total edges (after filter, undirected): %d\n", totalWeightedEdges))
	b.WriteString(fmt.Sprintf("Communities found: %d (showing %d, min size %d)\n", len(rawCommunities), len(infos), args.MinSize))
	b.WriteString(fmt.Sprintf("Modularity Q: %.4f\n\n", q))

	for i, info := range infos {
		idx := i + 1
		b.WriteString(fmt.Sprintf("## Community %d (id=%d) — %d nodes, %d internal edges\n",
			idx, info.ID, info.Size, info.InternalEdges))

		if len(info.KindDist) > 0 {
			var kindParts []string
			for kind, cnt := range info.KindDist {
				kindParts = append(kindParts, fmt.Sprintf("%s:%d", kind, cnt))
			}
			sort.Strings(kindParts)
			b.WriteString(fmt.Sprintf("Kinds: %s\n", strings.Join(kindParts, ", ")))
		}

		if len(info.TopFiles) > 0 {
			b.WriteString(fmt.Sprintf("Top files: %s\n", strings.Join(info.TopFiles, ", ")))
		}

		if len(info.TopSymbols) > 0 {
			b.WriteString("Top symbols:\n")
			maxShow := 10
			if len(info.TopSymbols) < maxShow {
				maxShow = len(info.TopSymbols)
			}
			for _, s := range info.TopSymbols[:maxShow] {
				relFile := db.RelPath(workdir, s.File)
				b.WriteString(fmt.Sprintf("  %s (%s:%d  w=%.2f)\n", s.Name, relFile, s.Line, s.WeightedDegree))
			}
		}
		b.WriteString("\n")
	}

	return &CommunityResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

// buildCommunityInfo computes per-community statistics.
//
// Parameters:
//   - id: community index in the Louvain result
//   - nodes: community members as graph.Node with compact IDs
//   - dbIDs: DB node IDs corresponding to each member (same order as nodes)
//   - nodeIndex: DB node ID → compact ID
//   - revIndex: compact ID → db.Node
//   - snapshot: full graph snapshot (for edge scans)
func buildCommunityInfo(id int, nodes []graph.Node, dbIDs []int64,
	nodeIndex map[int64]int64, revIndex map[int64]db.Node,
	snapshot *db.GraphSnapshot) CommunityInfo {

	// Build a set of compact IDs for O(1) membership tests.
	compactSet := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		compactSet[n.ID()] = true
	}

	// Count internal edges and compute weighted degrees for this community.
	// weightedDeg is keyed by DB node ID so we can look up db.Node later.
	weightedDeg := make(map[int64]float64)
	internalEdges := 0

	for _, e := range snapshot.Edges {
		if e.Kind == db.EdgeContains {
			continue
		}
		srcC, srcOK := nodeIndex[e.SourceID]
		tgtC, tgtOK := nodeIndex[e.TargetID]
		if !srcOK || !tgtOK {
			continue
		}
		// Both endpoints in this community?
		if compactSet[srcC] && compactSet[tgtC] {
			internalEdges++
		}
		// Accumulate weighted degree (undirected: add to both).
		if compactSet[srcC] {
			weightedDeg[e.SourceID] += provenanceWeight[e.Provenance]
		}
		if compactSet[tgtC] && e.SourceID != e.TargetID {
			weightedDeg[e.TargetID] += provenanceWeight[e.Provenance]
		}
	}

	// Sort community nodes by weighted degree descending (stable for ties).
	sort.Slice(dbIDs, func(i, j int) bool {
		wi := weightedDeg[dbIDs[i]]
		wj := weightedDeg[dbIDs[j]]
		if wi != wj {
			return wi > wj
		}
		// Tie-break by name for deterministic output.
		ni := revIndex[nodeIndex[dbIDs[i]]]
		nj := revIndex[nodeIndex[dbIDs[j]]]
		return ni.Name < nj.Name
	})

	// Build top symbols, kind distribution, and file set.
	topSymbols := make([]topSymbol, 0, len(dbIDs))
	fileSet := make(map[string]int)
	kindDist := make(map[string]int)

	for _, dbID := range dbIDs {
		compactID := nodeIndex[dbID]
		n, ok := revIndex[compactID]
		if !ok {
			continue
		}
		topSymbols = append(topSymbols, topSymbol{
			Name:           n.Name,
			File:           n.File,
			Line:           n.Line,
			WeightedDegree: weightedDeg[dbID],
		})
		if n.File != "" {
			fileSet[n.File]++
		}
		if n.Kind != "" {
			kindDist[n.Kind]++
		}
	}

	// Top files by symbol count.
	type fileCnt struct {
		file string
		cnt  int
	}
	var fcList []fileCnt
	for f, c := range fileSet {
		fcList = append(fcList, fileCnt{file: f, cnt: c})
	}
	sort.Slice(fcList, func(i, j int) bool {
		if fcList[i].cnt != fcList[j].cnt {
			return fcList[i].cnt > fcList[j].cnt
		}
		return fcList[i].file < fcList[j].file
	})
	topFiles := make([]string, 0, 5)
	for _, fc := range fcList {
		topFiles = append(topFiles, filepath.Base(fc.file))
		if len(topFiles) >= 5 {
			break
		}
	}

	return CommunityInfo{
		ID:            id,
		Size:          len(nodes),
		InternalEdges: internalEdges,
		TopSymbols:    topSymbols,
		TopFiles:      topFiles,
		KindDist:      kindDist,
	}
}

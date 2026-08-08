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

// maxCommunityNodes bounds the graph Louvain will run on. GetGraphSnapshot
// can return up to graphSnapshotCap (~500k) nodes; full-graph Louvain on
// that size burns CPU for a long time while holding the DB read lock (audit
// high: communities had no timeout and no size guard). 100k nodes covers
// every realistic project (≈20k files) while keeping a hostile/oversized
// index from pinning the daemon. A var so tests can lower it.
var maxCommunityNodes = 100_000

// ctxCheckInterval bounds how often the graph-build loops poll ctx: checking
// on every iteration would add a branch + atomic load per edge on a snapshot
// of up to ~500k rows, while every 4096 rows keeps the overhead negligible
// and a canceled request is still noticed within a single pass (worst case a
// few ms of extra work on a huge index).
const ctxCheckInterval = 4096

// ctxCheckEvery returns ctx.Err() at most once per ctxCheckInterval calls,
// i.e. periodically instead of on every iteration.
func ctxCheckEvery(ctx context.Context, i int) error {
	if i&(ctxCheckInterval-1) == 0 {
		return ctx.Err()
	}
	return nil
}

// maxCommunities caps reported communities (server layer clamps too; this
// is defense in depth for direct package use).
const maxCommunities = 100

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

// weightFor returns the edge weight for a provenance string, defaulting to 0.5
// for unknown/empty provenance (consistent everywhere).
func weightFor(provenance string) float64 {
	if w, ok := provenanceWeight[provenance]; ok {
		return w
	}
	return 0.5
}

// CommunityInfo holds one detected community for output formatting.
type CommunityInfo struct {
	ID            int            `json:"id"`
	Size          int            `json:"size"`
	InternalEdges int            `json:"internalEdges"`
	TopSymbols    []topSymbol    `json:"topSymbols"`
	TopFiles      []string       `json:"topFiles"`
	KindDist      map[string]int `json:"kindDist"`
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
	if args.Max > maxCommunities {
		args.Max = maxCommunities
	}
	if args.MinSize <= 0 {
		args.MinSize = 3
	}

	// Step 1: Load graph snapshot. GetGraphSnapshot has no ctx variant (the
	// DB side is not interruptible), so check ctx before and after the call
	// and poll periodically through the build loops below — a canceled
	// request must abort during graph construction, not only at the Louvain
	// gates. The 60s server deadline therefore bounds the build + enrichment
	// phase for real, not just the tail ends.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := database.GetGraphSnapshot()
	if err != nil {
		return nil, fmt.Errorf("load graph snapshot: %w", err)
	}

	if len(snapshot.Nodes) == 0 {
		return &CommunityResult{
			Content: []ContentItem{{Type: "text", Text: "No nodes in index. Nothing to analyze."}},
		}, nil
	}

	// Node-scale guard (audit high): refuse before the O(E) graph build and
	// Louvain pass instead of burning CPU on an oversized index. The DB
	// snapshot itself is already capped at ~500k rows; this keeps the
	// expensive part bounded.
	if len(snapshot.Nodes) > maxCommunityNodes {
		return nil, fmt.Errorf("community detection refused: index has %d nodes (max %d) — the full-graph Louvain pass would take too long; ask the operator to analyze a smaller index", len(snapshot.Nodes), maxCommunityNodes)
	}

	// Cancellation gate: the Louvain pass itself is not interruptible and
	// the snapshot is expensive, so refuse a canceled request before
	// building the graph.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 2: Build node ID mapping (DB node id → 0-based compact ID for gonum)
	// and reverse mapping (compact ID → db.Node for output enrichment).
	nodeIndex := make(map[int64]int64, len(snapshot.Nodes))      // DB node ID → compact ID
	reverseIndex := make(map[int64]db.Node, len(snapshot.Nodes)) // compact ID → DB node
	g := simple.NewWeightedUndirectedGraph(0, 0)
	for i, n := range snapshot.Nodes {
		if err := ctxCheckEvery(ctx, i); err != nil {
			return nil, err
		}
		cid := int64(i)
		nodeIndex[n.ID] = cid
		reverseIndex[cid] = n
		g.AddNode(simple.Node(cid))
	}

	// Step 3: Add weighted edges.
	// Filter out 'contains' edges, project directed→undirected, accumulate weight by provenance.
	edgeWeights := make(map[[2]int64]float64)
	for i, e := range snapshot.Edges {
		if err := ctxCheckEvery(ctx, i); err != nil {
			return nil, err
		}
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
		edgeWeights[[2]int64{sid, tid}] += weightFor(e.Provenance)
	}

	totalWeightedEdges := 0
	i := 0
	for pair, w := range edgeWeights {
		if err := ctxCheckEvery(ctx, i); err != nil {
			return nil, err
		}
		g.SetWeightedEdge(simple.WeightedEdge{
			F: simple.Node(pair[0]),
			T: simple.Node(pair[1]),
			W: w,
		})
		totalWeightedEdges++
		i++
	}

	// Final cancellation gate before the (non-interruptible) Louvain pass.
	if err := ctx.Err(); err != nil {
		return nil, err
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

	// Step 5: Enrich each community with statistics. buildCommunityInfo scans
	// all edges per community (up to maxCommunities × snapshot edges), so it
	// polls ctx periodically too.
	infos := make([]CommunityInfo, 0, len(commList))
	for _, ce := range commList {
		info, err := buildCommunityInfo(ctx, ce.id, ce.nodes, ce.dbIDs, nodeIndex, reverseIndex, snapshot)
		if err != nil {
			return nil, err
		}
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

// buildCommunityInfo computes per-community statistics. It returns an error
// when ctx is canceled mid-scan (the per-community edge pass is O(edges) and
// can dominate runtime when many communities are reported).
//
// Parameters:
//   - id: community index in the Louvain result
//   - nodes: community members as graph.Node with compact IDs
//   - dbIDs: DB node IDs corresponding to each member (same order as nodes)
//   - nodeIndex: DB node ID → compact ID
//   - revIndex: compact ID → db.Node
//   - snapshot: full graph snapshot (for edge scans)
func buildCommunityInfo(ctx context.Context, id int, nodes []graph.Node, dbIDs []int64,
	nodeIndex map[int64]int64, revIndex map[int64]db.Node,
	snapshot *db.GraphSnapshot) (CommunityInfo, error) {

	// Build a set of compact IDs for O(1) membership tests.
	compactSet := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		compactSet[n.ID()] = true
	}

	// Count internal edges and compute weighted degrees for this community.
	// weightedDeg is keyed by DB node ID so we can look up db.Node later.
	weightedDeg := make(map[int64]float64)
	internalEdges := 0

	for i, e := range snapshot.Edges {
		if err := ctxCheckEvery(ctx, i); err != nil {
			return CommunityInfo{}, err
		}
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
			weightedDeg[e.SourceID] += weightFor(e.Provenance)
		}
		if compactSet[tgtC] && e.SourceID != e.TargetID {
			weightedDeg[e.TargetID] += weightFor(e.Provenance)
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
	}, nil
}

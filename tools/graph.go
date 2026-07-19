package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
	"github.com/dorokuma/codegraph-go/extraction"
)

// GraphQueryArgs is shared by callers / callees / impact.
type GraphQueryArgs struct {
	Name       string `json:"name"`
	Path       string `json:"path,omitempty"`
	File       string `json:"file,omitempty"` // pin definition to this file (path/basename) when overloaded
	Glob       string `json:"glob,omitempty"` // filter defs/hits by filepath.Match on relative path
	MaxResults int    `json:"max_results,omitempty"`
	Depth      int    `json:"depth,omitempty"` // impact / multi-hop
}

// ExploreArgs drives the primary explore tool (official-style).
type ExploreArgs struct {
	Query    string `json:"query,omitempty"`    // symbol names / free text; empty = overview
	Path     string `json:"path,omitempty"`     // optional project subdir (home mode)
	Max      int    `json:"max,omitempty"`
	SkipCode bool   `json:"skipCode,omitempty"` // when true, omit source bodies; show location + trail only
}

// ToolCallersGraph finds call sites via the indexed call graph.
// Returns (text, found). found=false means the index has no edges — caller may fall back to rg.
func ToolCallersGraph(database *db.DB, workdir string, args GraphQueryArgs) (string, bool, error) {
	if args.Name == "" {
		return "", false, fmt.Errorf("name is required")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 40
	}

	defs, err := resolveDefs(database, args.Name, args.Path, args.File, args.Glob, workdir)
	if err != nil {
		return "", false, err
	}
	if len(defs) == 0 {
		return "", false, nil
	}

	var b strings.Builder
	total := 0
	anyEdge := false

	for _, def := range defs {
		callers, err := database.GetCallersWithKind(def.ID)
		if err != nil {
			return "", false, err
		}
		if len(callers) == 0 {
			continue
		}
		anyEdge = true
		for _, c := range callers {
			if total >= args.MaxResults {
				fmt.Fprintf(&b, "... (max %d)\n", args.MaxResults)
				return b.String(), true, nil
			}
			fmt.Fprintf(&b, "%s:%d\n", db.RelPath(workdir, c.File), c.Line)
			total++
		}
	}

	if !anyEdge {
		return "", false, nil
	}
	return b.String(), true, nil
}

// ToolCalleesGraph lists what a symbol calls via the indexed call graph.
func ToolCalleesGraph(database *db.DB, workdir string, args GraphQueryArgs) (string, bool, error) {
	if args.Name == "" {
		return "", false, fmt.Errorf("name is required")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 40
	}

	defs, err := resolveDefs(database, args.Name, args.Path, args.File, args.Glob, workdir)
	if err != nil {
		return "", false, err
	}
	if len(defs) == 0 {
		return "", false, nil
	}

	var b strings.Builder
	total := 0
	anyEdge := false

	for _, def := range defs {
		callees, err := database.GetCalleesWithKind(def.ID)
		if err != nil {
			return "", false, err
		}
		if len(callees) == 0 {
			continue
		}
		anyEdge = true
		seen := map[string]bool{}
		for _, c := range callees {
			key := fmt.Sprintf("%s|%s|%d", c.Name, c.File, c.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			if total >= args.MaxResults {
				fmt.Fprintf(&b, "... (max %d)\n", args.MaxResults)
				return b.String(), true, nil
			}
			fmt.Fprintf(&b, "%s:%d\n", db.RelPath(workdir, c.File), c.Line)
			total++
		}
	}

	if !anyEdge {
		return "", false, nil
	}
	return b.String(), true, nil
}

// ToolImpactGraph returns files affected by changing a symbol (via call/import edges).
func ToolImpactGraph(database *db.DB, workdir string, args GraphQueryArgs) (string, bool, error) {
	if args.Name == "" {
		return "", false, fmt.Errorf("name is required")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 40
	}
	depth := args.Depth
	if depth <= 0 {
		depth = 2
	}
	if depth > 5 {
		depth = 5
	}

	defs, err := resolveDefs(database, args.Name, args.Path, args.File, args.Glob, workdir)
	if err != nil {
		return "", false, err
	}
	if len(defs) == 0 {
		return "", false, nil
	}

	// BFS over reverse call edges (who depends on me)
	type hit struct {
		file  string
		count int
		depth int
	}
	fileHits := map[string]*hit{}
	visited := map[int64]bool{}
	queue := make([]struct {
		id  int64
		d   int
	}, 0, len(defs))
	for _, d := range defs {
		queue = append(queue, struct {
			id int64
			d  int
		}{d.ID, 0})
		visited[d.ID] = true
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.d >= depth {
			continue
		}
		callers, err := database.GetCallers(cur.id)
		if err != nil {
			return "", false, err
		}
		for _, c := range callers {
			h := fileHits[c.File]
			if h == nil {
				h = &hit{file: c.File, depth: cur.d + 1}
				fileHits[c.File] = h
			}
			h.count++
			if !visited[c.ID] {
				visited[c.ID] = true
				queue = append(queue, struct {
					id int64
					d  int
				}{c.ID, cur.d + 1})
			}
		}
	}

	if len(fileHits) == 0 {
		return "", false, nil
	}

	list := make([]*hit, 0, len(fileHits))
	for _, h := range fileHits {
		list = append(list, h)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].file < list[j].file
	})

	var b strings.Builder
	for i, h := range list {
		if i >= args.MaxResults {
			fmt.Fprintf(&b, "... (max %d)\n", args.MaxResults)
			break
		}
		fmt.Fprintf(&b, "%s\n", db.RelPath(workdir, h.file))
	}
	return b.String(), true, nil
}

// ToolExplore builds an overview or a symbol-centered context pack (official primary tool).
// With a multi-symbol query it surfaces Flow (call path) first, then source under a size budget.
func ToolExplore(ctx context.Context, database *db.DB, workdir string, args ExploreArgs) (string, error) {
	_ = ctx

	root := workdir
	if args.Path != "" {
		p := args.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(workdir, p)
		}
		p = filepath.Clean(p)
		if p == workdir || strings.HasPrefix(p, workdir+string(filepath.Separator)) {
			root = p
		}
	}

	query := strings.TrimSpace(args.Query)
	if query == "" {
		max := args.Max
		if max <= 0 {
			max = 30
		}
		return exploreOverview(database, workdir, root, max)
	}
	return exploreQuery(database, workdir, root, query, args.Max, args.SkipCode)
}

func exploreOverview(database *db.DB, workdir, root string, max int) (string, error) {
	var b strings.Builder

	// Home / broad workdir: list indexed project-like top-level dirs.
	if extraction.IsBroadWorkdir(workdir) && root == workdir {
		fmt.Fprintf(&b, "# CodeGraph home-mode overview\n")
		fmt.Fprintf(&b, "Index root: %s (only project-like top-level dirs)\n\n", workdir)
		fmt.Fprintf(&b, "## Projects\n")
		entries, err := os.ReadDir(workdir)
		if err != nil {
			return "", err
		}
		n := 0
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(workdir, e.Name())
			if extraction.ShouldSkipDirIn(workdir, full, e.Name()) {
				continue
			}
			if !extraction.HasProjectMarker(full) {
				continue
			}
			fmt.Fprintf(&b, "- %s/\n", e.Name())
			n++
			if n >= max {
				break
			}
		}
		if n == 0 {
			b.WriteString("(no project markers found)\n")
		}
		b.WriteString("\nTip: pass path=<project> or query=<symbol> for a focused explore.\n")

		if stats, err := database.GetStats(); err == nil && stats != nil {
			fmt.Fprintf(&b, "\n## Index\n- nodes: %d · edges: %d · files: %d\n", stats.NodeCount, stats.EdgeCount, stats.FileCount)
		}
		return b.String(), nil
	}

	fmt.Fprintf(&b, "# Explore %s\n\n", db.RelPath(workdir, root))
	fmt.Fprintf(&b, "## Top-level\n")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		fmt.Fprintf(&b, "- %s%s\n", e.Name(), suffix)
		n++
		if n >= max {
			break
		}
	}

	// Manifests / docs inside root
	fmt.Fprintf(&b, "\n## Manifests & docs\n")
	manifests := []string{
		"README.md", "README", "go.mod", "package.json", "pyproject.toml",
		"Cargo.toml", "pom.xml", "build.gradle", "Makefile", "composer.json",
	}
	shown := 0
	for _, m := range manifests {
		p := filepath.Join(root, m)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			fmt.Fprintf(&b, "- %s\n", db.RelPath(workdir, p))
			shown++
		}
	}
	if shown == 0 {
		b.WriteString("(none at top level)\n")
	}

	// Sample of indexed symbols in this subtree
	if files, err := database.ListFiles(); err == nil {
		prefix := root
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		count := 0
		for _, f := range files {
			if f == root || strings.HasPrefix(f, prefix) {
				count++
			}
		}
		fmt.Fprintf(&b, "\n## Indexed under this path\n- files: %d\n", count)
	}

	b.WriteString("\nTip: explore with query=\"SymbolName\" to pull source + callers/callees in one shot.\n")
	return b.String(), nil
}

func exploreQuery(database *db.DB, workdir, root, query string, max int, skipCode bool) (string, error) {
	fileCount := 0
	if stats, err := database.GetStats(); err == nil && stats != nil {
		fileCount = stats.FileCount
	}
	budget := GetExploreOutputBudget(fileCount)
	maxFiles := max
	if maxFiles <= 0 {
		maxFiles = budget.DefaultMaxFiles
	}
	// Hard safety only — honor explicit Max (no silent clamp at 30).
	const exploreMaxFilesCeiling = 100
	if maxFiles > exploreMaxFilesCeiling {
		maxFiles = exploreMaxFilesCeiling
	}
	maxEdges := budget.MaxEdgesPerRelationshipKind
	if maxEdges <= 0 {
		maxEdges = 8
	}

	// 1) Flow among named symbols (bag of tokens); same path/root as source gather.
	flow := buildFlowFromNamedSymbols(database, workdir, root, query)

	// 2) Gather symbols to show: flow spine/named first, then classic lookup.
	nodes, err := gatherExploreNodes(database, workdir, root, query, flow, maxFiles*4)
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		return fmt.Sprintf("no indexed symbols matched %q under %s\nTry search, or check status for pending sync.", query, db.RelPath(workdir, root)), nil
	}

	// Group by file; rank named/spine files first.
	type fileGroup struct {
		file  string
		nodes []db.Node
		score int
	}
	byFile := map[string]*fileGroup{}
	var groups []*fileGroup
	for _, n := range nodes {
		g := byFile[n.File]
		if g == nil {
			g = &fileGroup{file: n.File}
			byFile[n.File] = g
			groups = append(groups, g)
		}
		g.nodes = append(g.nodes, n)
		if flow.PathNodeIDs[n.ID] {
			g.score += 100
		}
		if flow.NamedNodeIDs[n.ID] || flow.UniqueNamedNodeIDs[n.ID] {
			g.score += 50
		}
		if isExploreCallable(n.Kind) {
			g.score += 1
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].score != groups[j].score {
			return groups[i].score > groups[j].score
		}
		return groups[i].file < groups[j].file
	})

	var b strings.Builder
	fmt.Fprintf(&b, "# Explore %q\n", query)
	fmt.Fprintf(&b, "Matched %d symbol(s) across up to %d files. Source below is from the index — treat as already read.\n\n", len(nodes), maxFiles)

	// Flow first (product reason to prefer explore over ad-hoc Read/Grep).
	if flow.Text != "" {
		b.WriteString(flow.Text)
	}

	totalChars := b.Len()
	filesIncluded := 0
	var skippedFiles []string
	hardCeiling := budget.MaxOutputChars * 3 / 2
	if hardCeiling > 25000 {
		hardCeiling = 25000
	}
	if hardCeiling < budget.MaxOutputChars {
		hardCeiling = budget.MaxOutputChars
	}

	for _, g := range groups {
		if filesIncluded >= maxFiles {
			skippedFiles = append(skippedFiles, g.file)
			continue
		}
		necessary := false
		for _, n := range g.nodes {
			if flow.PathNodeIDs[n.ID] || flow.NamedNodeIDs[n.ID] || flow.UniqueNamedNodeIDs[n.ID] {
				necessary = true
				break
			}
		}
		if budget.ExcludeLowValueFiles && !necessary && isLowValueExploreFile(g.file) {
			skippedFiles = append(skippedFiles, g.file)
			continue
		}
		// Past 90% budget: only keep necessary (named/spine) files.
		if !necessary && totalChars > budget.MaxOutputChars*9/10 {
			skippedFiles = append(skippedFiles, g.file)
			continue
		}

		var section strings.Builder
		fmt.Fprintf(&section, "## %s\n", db.RelPath(workdir, g.file))
		// Compact symbol header
		headerN := 0
		for _, n := range g.nodes {
			if headerN >= budget.MaxSymbolsInFileHeader {
				break
			}
			if headerN == 0 {
				section.WriteString("— ")
			} else {
				section.WriteString(", ")
			}
			fmt.Fprintf(&section, "%s(%s)", n.Name, n.Kind)
			headerN++
		}
		if headerN > 0 {
			section.WriteString("\n")
		}

		fileBodyBudget := budget.MaxCharsPerFile
		remaining := budget.MaxOutputChars - totalChars - 200
		if remaining < 0 {
			remaining = 0
		}
		if necessary {
			// Spine/named files get a bit more room but still bounded.
			spineCap := budget.MaxCharsPerFile*5/2
			if spineCap > remaining && remaining > 0 {
				spineCap = remaining
			}
			if spineCap > fileBodyBudget {
				fileBodyBudget = spineCap
			}
		} else if fileBodyBudget > remaining {
			fileBodyBudget = remaining
		}
		usedInFile := 0

		for _, n := range g.nodes {
			var nb strings.Builder
			fmt.Fprintf(&nb, "\n### %s (%s) L%d\n", n.Name, n.Kind, n.Line)

			if callers, err := database.GetCallersWithKind(n.ID); err == nil && len(callers) > 0 {
				nb.WriteString("Callers: ")
				parts := make([]string, 0, min(maxEdges, len(callers)))
				for i, c := range callers {
					if i >= maxEdges {
						parts = append(parts, "…")
						break
					}
					label := fmt.Sprintf("%s@%s:%d", c.Name, filepath.Base(c.File), c.Line)
					if c.EdgeKind == db.EdgeReferences {
						label += "(route)"
					} else if c.EdgeKind == "bridge" {
						label += "(bridge)"
					}
					parts = append(parts, label)
				}
				nb.WriteString(strings.Join(parts, ", "))
				nb.WriteByte('\n')
			}
			if callees, err := database.GetCalleesWithKind(n.ID); err == nil && len(callees) > 0 {
				nb.WriteString("Calls: ")
				parts := make([]string, 0, min(maxEdges, len(callees)))
				seen := map[string]bool{}
				for _, c := range callees {
					key := c.Name + c.EdgeKind
					if seen[key] {
						continue
					}
					seen[key] = true
					if len(parts) >= maxEdges {
						parts = append(parts, "…")
						break
					}
					label := c.Name
					if c.EdgeKind == db.EdgeReferences {
						label += "(handler)"
					} else if c.EdgeKind == "bridge" {
						label += "(bridge)"
					}
					parts = append(parts, label)
				}
				nb.WriteString(strings.Join(parts, ", "))
				nb.WriteByte('\n')
			}

			body := n.Body
			if body != "" {
				if skipCode {
					lines := strings.Count(body, "\n") + 1
					fmt.Fprintf(&nb, "[ %d lines of %s ]\n", lines, n.Language)
				} else {
					// Leave room for fences inside the per-file budget.
					room := fileBodyBudget - usedInFile - nb.Len() - 32
					if room < 80 && usedInFile > 0 && !flow.PathNodeIDs[n.ID] {
						continue
					}
					if room > 0 && len(body) > room {
						body = trimExploreBody(body, room)
					}
					lang := n.Language
					if lang == "" {
						lang = "text"
					}
					fmt.Fprintf(&nb, "```%s\n%s\n```\n", lang, body)
				}
			}

			chunk := nb.String()
			if !necessary && totalChars+section.Len()+len(chunk)+200 > budget.MaxOutputChars {
				break
			}
			section.WriteString(chunk)
			usedInFile += len(chunk)
			if usedInFile >= fileBodyBudget {
				break
			}
		}

		sec := section.String()
		if !necessary && totalChars+len(sec)+200 > budget.MaxOutputChars {
			skippedFiles = append(skippedFiles, g.file)
			continue
		}
		b.WriteString("\n")
		b.WriteString(sec)
		totalChars = b.Len()
		filesIncluded++
		if totalChars >= hardCeiling {
			break
		}
	}

	if budget.IncludeAdditionalFiles && len(skippedFiles) > 0 {
		b.WriteString("\n## Additional relevant files (not shown)\n")
		for i, f := range skippedFiles {
			if i >= 12 {
				fmt.Fprintf(&b, "- … +%d more\n", len(skippedFiles)-i)
				break
			}
			fmt.Fprintf(&b, "- %s\n", db.RelPath(workdir, f))
		}
	}
	if budget.IncludeCompletenessSignal {
		b.WriteString("\nComplete source for the symbols above is included — treat as already read; prefer node only if you need one whole file.\n")
	}
	if budget.IncludeBudgetNote {
		fmt.Fprintf(&b, "\nExplore budget: up to %d call(s) recommended for ~%d indexed files (this reply capped near %d chars).\n",
			GetExploreBudget(fileCount), fileCount, budget.MaxOutputChars)
	}

	// Last-resort hard ceiling so hosts never externalize a giant tool result.
	out := b.String()
	if len(out) > hardCeiling {
		out = out[:hardCeiling] + "\n… (explore output truncated to budget ceiling)\n"
	}
	return out, nil
}

// gatherExploreNodes collects symbols for explore output: multi-token bag,
// flow spine/named, then single-name / FTS fallback.
func gatherExploreNodes(database *db.DB, workdir, root, query string, flow flowResult, capN int) ([]db.Node, error) {
	if capN <= 0 {
		capN = 30
	}
	seen := map[int64]bool{}
	var out []db.Node
	add := func(n db.Node) {
		if n.Kind == db.KindFile || n.Kind == "module" {
			return
		}
		if !inExploreRoot(n.File, workdir, root) {
			return
		}
		if seen[n.ID] {
			return
		}
		seen[n.ID] = true
		out = append(out, n)
	}

	// Spine order first (the Flow answer).
	for _, n := range flow.PathNodes {
		add(n)
	}
	for _, n := range flow.NamedNodes {
		add(n)
	}

	tokens := tokenizeExploreQuery(query)
	if len(tokens) == 0 {
		tokens = []string{strings.TrimSpace(query)}
	}
	// Multi-token: resolve each token; single token: classic exact → FTS.
	for _, t := range tokens {
		if len(out) >= capN {
			break
		}
		hits, err := findCallableSymbols(database, t)
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			// non-callable symbols (types, etc.) still useful for single-name explore
			nodes, err := database.GetNodeByName(t)
			if err != nil {
				return nil, err
			}
			if len(nodes) == 0 {
				if i := strings.LastIndexAny(t, ".#/"); i >= 0 && i+1 < len(t) {
					nodes, err = database.GetNodeByName(t[i+1:])
					if err != nil {
						return nil, err
					}
				}
			}
			hits = nodes
		}
		for _, n := range hits {
			add(n)
			if len(out) >= capN {
				break
			}
		}
	}

	// Single-token / no hits: FTS on full query string (legacy behavior).
	if len(out) == 0 {
		nodes, err := database.FullTextSearch(query, capN)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			add(n)
		}
	}
	return out, nil
}

func trimExploreBody(body string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if len(body) <= maxChars {
		return body
	}
	// Prefer keeping the head (signature + start of body).
	cut := maxChars
	if cut > 40 {
		// try not to split mid-line
		if i := strings.LastIndex(body[:cut], "\n"); i > cut/2 {
			cut = i
		}
	}
	return body[:cut] + "\n… (trimmed to explore budget)"
}

func resolveDefs(database *db.DB, name, pathFilter, fileHint, glob, workdir string) ([]db.Node, error) {
	nodes, err := database.GetNodeByName(name)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		// FTS fallback for partial names
		nodes, err = database.FullTextSearch(name, 20)
		if err != nil {
			return nil, err
		}
		// Keep exact name matches only from FTS noise
		var exact []db.Node
		for _, n := range nodes {
			if n.Name == name || strings.HasSuffix(n.Name, "."+name) {
				exact = append(exact, n)
			}
		}
		if len(exact) > 0 {
			nodes = exact
		}
	}

	var defs []db.Node
	for _, n := range nodes {
		if n.Kind == db.KindFile || n.Kind == "module" {
			continue
		}
		if pathFilter != "" {
			p := pathFilter
			if !filepath.IsAbs(p) {
				p = filepath.Join(workdir, p)
			}
			p = filepath.Clean(p)
			prefix := p
			if !strings.HasSuffix(prefix, string(filepath.Separator)) {
				prefix += string(filepath.Separator)
			}
			if n.File != p && !strings.HasPrefix(n.File, prefix) {
				continue
			}
		}
		if glob != "" {
			rel := db.RelPath(workdir, n.File)
			ok, err := filepath.Match(glob, rel)
			if err != nil || !ok {
				// also try basename-only patterns like *.go
				ok2, err2 := filepath.Match(glob, filepath.Base(n.File))
				if err2 != nil || !ok2 {
					continue
				}
			}
		}
		defs = append(defs, n)
	}

	// file hint pins overloaded names (path/basename/boundary suffix); only narrows.
	if fileHint != "" && len(defs) > 1 {
		var byFile []db.Node
		for _, n := range defs {
			if fileHintMatches(n.File, fileHint, workdir) {
				byFile = append(byFile, n)
			}
		}
		if len(byFile) > 0 {
			defs = byFile
		}
	}
	return defs, nil
}

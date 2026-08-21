package extraction

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// splitNameLineKey parses keys produced as fmt.Sprintf("%s:%d", name, line).
func splitNameLineKey(key string) (name string, line int, ok bool) {
	// name may contain ':' (rare); split from the right.
	i := len(key) - 1
	for i >= 0 && key[i] >= '0' && key[i] <= '9' {
		i--
	}
	if i < 0 || key[i] != ':' || i+1 >= len(key) {
		return "", 0, false
	}
	n := 0
	for _, c := range key[i+1:] {
		n = n*10 + int(c-'0')
	}
	return key[:i], n, true
}

// bareRank scores node kinds for same-file bare-name lookup.
// Higher wins so listUsers(function) beats a coincidental non-callable.

// bareRank scores node kinds for same-file bare-name lookup.
// Higher wins so listUsers(function) beats a coincidental non-callable.
func bareRank(kind string) int {
	switch kind {
	case "function", "method", "component", "constructor":
		return 3
	case "class", "struct", "interface", "type":
		return 2
	case "route":
		return 1
	default:
		return 0
	}
}

// simplifyHandlerName turns framework handler expressions into a bare symbol.
// Examples: listUsers, pkg.Handler, (*User).Create, UsersController@index, h.Serve

// simplifyHandlerName turns framework handler expressions into a bare symbol.
// Examples: listUsers, pkg.Handler, (*User).Create, UsersController@index, h.Serve
func simplifyHandlerName(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return h
	}
	// Laravel-style Controller@action
	if i := strings.LastIndex(h, "@"); i >= 0 && i+1 < len(h) {
		return h[i+1:]
	}
	// Strip (*T).Method / T.Method / pkg.Func
	if i := strings.LastIndex(h, "."); i >= 0 && i+1 < len(h) {
		return strings.Trim(h[i+1:], "() ")
	}
	// Drop trailing ()
	h = strings.TrimSuffix(h, "()")
	return strings.Trim(h, " ")
}

// linkSameFileOrPark was replaced by the batched indexFile (A4): same-file
// links and cross-file parks are now collected and written atomically in
// ReplaceFileIndex. Keep parkUnresolved's logic inline there (see link()).

// maxIndexFileSize skips oversized blobs (minified bundles, generated dumps).

// indexFile extracts and writes the index for one file. data, when non-nil,
// is the file content already read by the caller (indexIfNeeded's content-hash
// gate, F4) and is reused instead of reading the file a second time; nil means
// read from disk. contentHash is always derived from the same bytes that get
// indexed, so the hash gate and the stored hash can never disagree.
func (o *Orchestrator) indexFile(path string, lang string, data []byte) (int, error) {
	if data == nil {
		var err error
		data, err = o.readFile(path)
		if err != nil {
			return 0, err
		}
	}
	contentHash := hashContent(data)
	// Portable index key: workdir-relative.
	store := o.storePath(path)

	// Content-hash short-circuit: mtime can change without edits (touch, checkout).
	// Skip clear+re-extract when the bytes are identical (unless force rebuild).
	if !o.isForce() {
		if same, herr := o.db.FileHasContentHash(store, contentHash); herr == nil && same {
			if info, serr := os.Stat(path); serr == nil {
				// Refresh size/mtime so the cheap FileNeedsReindex stays quiet.
				_ = o.db.TouchFileMeta(store, info.Size(), float64(info.ModTime().UnixMilli()), contentHash)
			}
			return 0, nil
		}
	}

	// Extract: tree-sitter first, regex fallback. On total failure the previous
	// index is KEPT (no ClearFile, no empty file record) and IndexAll
	// continues with other files (A4/Critical#3). The file meta is deliberately
	// NOT touched on this path (M2): a fresh content_hash would make the
	// gates treat the file as current, so an unchanged broken file would be
	// skipped forever. Leaving the stale meta makes the next IndexAll retry
	// the file (self-healing). ErrKeepOldIndex is non-fatal (M7).
	// Normalize once for every consumer (extractors, route detection, bridge
	// detection): strip BOM and normalize CRLF so anchors/line math agree.
	source := normalizeSource(string(data))
	result, tsErrored, extractErr := o.extractFile(lang, source, store)
	// T5: tree-sitter TIMEOUT is not a parse verdict — the regex fallback can
	// yield partial/noisy results that would silently degrade a previously
	// complete tree-sitter index (and the fresh content hash would stop any
	// retry, so the quality loss would be permanent). Keep the old index when
	// one exists; only a file with no prior index gets the weak fallback
	// written (something beats nothing on the first pass).
	if errors.Is(extractErr, ErrTSParseTimeout) {
		if old, cerr := o.fileNodeCount(store); cerr != nil || old > 0 {
			log.Printf("warning: tree-sitter parse timed out for %s, keeping existing index", path)
			// No meta touch (M2): the stale meta makes the next IndexAll
			// retry the file; ErrKeepOldIndex keeps the pass going (M7).
			return 0, ErrKeepOldIndex
		}
		extractErr = nil
	}
	if extractErr != nil {
		log.Printf("warning: extraction failed for %s: %v (keeping existing index)", path, extractErr)
		return 0, ErrKeepOldIndex
	}
	// S1: tree-sitter reported a real parse error AND the regex fallback
	// found nothing (nodes+edges+refs all zero) while the file still has an
	// old index with symbols — the file is unparseable, not empty. Clearing
	// it would destroy the previous symbols, so treat it as an extraction
	// failure: keep the old index, log a warning, do NOT touch the file meta
	// (M2: the stale meta makes the next IndexAll retry the file) and return
	// ErrKeepOldIndex so IndexAll continues but the failure stays visible
	// (M7). The same applies when the stored node-count query itself fails
	// (cerr != nil): the old index could not be inspected, so never gamble
	// on clearing it. A successful tree-sitter parse with an empty result
	// (file genuinely cleared) still clears the old index as before.
	if tsErrored && len(result.Nodes) == 0 && len(result.Edges) == 0 && len(result.Refs) == 0 {
		if old, cerr := o.fileNodeCount(store); cerr != nil || old > 0 {
			log.Printf("warning: extraction failed for %s (tree-sitter error, regex found nothing), keeping existing index", path)
			// No meta touch here either (M2): the stale meta makes the next
			// IndexAll retry this file; ErrKeepOldIndex keeps the pass going.
			return 0, ErrKeepOldIndex
		}
	}
	nodes := result.Nodes
	edges := result.Edges
	refs := result.Refs

	// Detect framework routes (linked to handlers after node insert).
	detector := NewFrameworkDetector()
	routes := detector.DetectRoutes(source, store, lang)
	for _, route := range routes {
		handler := simplifyHandlerName(strings.TrimSpace(route.Handler))
		nodes = append(nodes, ExtractedNode{
			Kind:          "route",
			Name:          route.Method + " " + route.Path,
			File:          route.File,
			Line:          route.Line,
			EndLine:       route.Line,
			Body:          handler,
			Language:      lang,
			QualifiedName: route.QualifiedName,
		})
	}

	// ---- Build the atomic batch. File node first (index 0), then symbols. ----
	dbNodes := make([]db.Node, 0, len(nodes)+1)
	dbNodes = append(dbNodes, db.Node{
		Kind: db.KindFile, Name: store, File: store, Line: 0, Language: lang,
	})
	for _, n := range nodes {
		dbNodes = append(dbNodes, db.Node{
			Kind:          n.Kind,
			Name:          n.Name,
			File:          store,
			Line:          n.Line,
			EndLine:       n.EndLine,
			Body:          n.Body,
			Language:      lang,
			QualifiedName: n.QualifiedName,
			Signature:     n.Signature,
			Docstring:     n.Docstring,
			Visibility:    n.Visibility,
			IsExported:    n.IsExported,
			ReturnType:    n.ReturnType,
			StartColumn:   n.StartColumn,
			EndColumn:     n.EndColumn,
		})
	}
	// Dedup on the nodes conflict key (file,line,kind,name): UpsertNode would
	// merge duplicates into one row; keep the first occurrence so batch ids
	// stay 1:1 with dbNodes.
	{
		seen := make(map[string]struct{}, len(dbNodes))
		kept := dbNodes[:0]
		for _, n := range dbNodes {
			k := fmt.Sprintf("%s\x00%d\x00%s\x00%s", n.File, n.Line, n.Kind, n.Name)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			kept = append(kept, n)
		}
		dbNodes = kept
	}

	// name:line → dbNodes index; bare-name best-hit map (same ranking as before).
	// Names defined at more than one (line, rank) spot are ambiguous and are
	// deliberately NOT registered as bare-name hits: same-file linking must
	// never first-wins across two same-named symbols (audit: two String()
	// methods on different receivers stole each other's calls).
	nodeIdx := make(map[string]int, len(dbNodes))
	type bareHit struct{ idx, rank, line int }
	bareBest := map[string]bareHit{}
	ambiguous := map[string]bool{}
	nameIdx := map[string][]int{}
	for i := 1; i < len(dbNodes); i++ { // skip the file node
		n := dbNodes[i]
		nodeIdx[fmt.Sprintf("%s:%d", n.Name, n.Line)] = i
		if n.Kind == "signature" {
			// Interface method signatures can be neither a call source nor a
			// call target; keep them out of the link maps entirely.
			continue
		}
		nameIdx[n.Name] = append(nameIdx[n.Name], i)
		prev, ok := bareBest[n.Name]
		rank := bareRank(n.Kind)
		if ok && (prev.line != n.Line || prev.rank != rank) {
			ambiguous[n.Name] = true
		}
		if !ok || rank > prev.rank || (rank == prev.rank && n.Line < prev.line) {
			bareBest[n.Name] = bareHit{idx: i, rank: rank, line: n.Line}
		}
	}
	sameFileBare := make(map[string]int, len(bareBest))
	for name, hit := range bareBest {
		if ambiguous[name] {
			continue
		}
		// Bare name → best unambiguous hit (compatibility with older indexes).
		nodeIdx[name] = hit.idx
		sameFileBare[name] = hit.idx
	}

	// ph(i) is the batch placeholder id for dbNodes[i] (negative; resolved to
	// the real row id inside ReplaceFileIndex's transaction).
	ph := func(i int) int64 { return -int64(i + 1) }

	dbEdges := make([]db.Edge, 0, len(edges)+len(routes))
	var parks []db.UnresolvedRef
	link := func(from int, targetName string, line, col int, kind string) {
		if from < 0 || targetName == "" {
			return
		}
		if tid, ok := sameFileBare[targetName]; ok && tid > 0 {
			dbEdges = append(dbEdges, db.Edge{
				SourceID:   ph(from),
				TargetID:   ph(tid),
				Kind:       kind,
				File:       store,
				Line:       line,
				Col:        col,
				Provenance: "exact",
			})
			return
		}
		// Ambiguous same-file name (multiple definitions): link only when
		// exactly one definition's source range contains the reference line.
		// Any other case parks the ref so the resolution pass can decide
		// (and refuses when the candidates still tie).
		if idxs := nameIdx[targetName]; len(idxs) > 1 {
			hit := -1
			contain := 0
			for _, idx := range idxs {
				n := dbNodes[idx]
				if n.Kind == db.KindFile || n.Kind == "module" || n.Kind == "signature" {
					continue
				}
				end := n.EndLine
				if end == 0 {
					end = n.Line
				}
				if n.Line <= line && line <= end {
					contain++
					hit = idx
				}
			}
			if contain == 1 {
				dbEdges = append(dbEdges, db.Edge{
					SourceID:   ph(from),
					TargetID:   ph(hit),
					Kind:       kind,
					File:       store,
					Line:       line,
					Col:        col,
					Provenance: "exact",
				})
				return
			}
		}
		if !ShouldParkRef(o.db, targetName) {
			return
		}
		parks = append(parks, db.UnresolvedRef{
			FromNode:      ph(from),
			ReferenceName: targetName,
			ReferenceKind: kind,
			Line:          line,
			Col:           col,
			FilePath:      store,
			Language:      lang,
			Status:        "pending",
			NameTail:      NameTail(targetName),
		})
	}

	// Route → handler: same-file edge now; cross-file → unresolved_refs.
	for _, route := range routes {
		handler := simplifyHandlerName(route.Handler)
		if handler == "" {
			continue
		}
		routeKey := fmt.Sprintf("%s:%d", route.Method+" "+route.Path, route.Line)
		routeIdx, ok := nodeIdx[routeKey]
		if !ok {
			continue
		}
		link(routeIdx, handler, route.Line, 0, db.EdgeReferences)
	}

	// Pre-resolve import targets: module nodes live outside this file, so they
	// are looked up before the batch. New module nodes are NOT created here —
	// they are queued as placeholders and upserted inside ReplaceFileIndex's
	// transaction (F5), so a failed batch rolls back and leaves no orphaned
	// module nodes behind.
	importTarget := make(map[int]int64, len(edges))
	moduleNodes := make([]db.Node, 0, len(edges))
	moduleIdx := make(map[string]int, len(edges))
	for i, e := range edges {
		if e.Kind != "imports" {
			continue
		}
		targetNodes, gerr := o.db.GetNodeByName(e.TargetName)
		if gerr != nil {
			log.Printf("lookup import target %s: %v", e.TargetName, gerr)
		}
		var targetID int64
		if len(targetNodes) > 0 {
			// Prefer a module node so import edges don't attach to a
			// same-named function/class by accident.
			targetID = targetNodes[0].ID
			for _, tn := range targetNodes {
				if tn.Kind == "module" {
					targetID = tn.ID
					break
				}
			}
		} else {
			// Queue a module node for creation inside the batch transaction.
			// Its placeholder id -(len(dbNodes)+idx+1) is resolved to the real
			// row id by ReplaceFileIndex (module placeholders live just below
			// the batch-node placeholder range).
			idx, ok := moduleIdx[e.TargetName]
			if !ok {
				idx = len(moduleNodes)
				moduleIdx[e.TargetName] = idx
				moduleNodes = append(moduleNodes, db.Node{
					Kind:     "module",
					Name:     e.TargetName,
					File:     e.TargetName,
					Line:     0,
					Language: lang,
				})
			}
			targetID = -int64(len(dbNodes) + idx + 1)
		}
		if targetID != 0 {
			importTarget[i] = targetID
		}
	}

	// Structural edges (imports, etc.)
	for i, e := range edges {
		if e.Kind == "imports" {
			if tid := importTarget[i]; tid != 0 {
				dbEdges = append(dbEdges, db.Edge{
					SourceID:   ph(0),
					TargetID:   tid,
					Kind:       "imports",
					File:       store,
					Line:       e.Line,
					Provenance: "exact",
				})
			}
			continue
		}
		srcIdx := o.resolveSourceIdx(e.SourceName, e.Line, nodeIdx)
		if srcIdx < 0 {
			continue
		}
		if e.Kind == "calls" {
			link(srcIdx, e.TargetName, e.Line, e.Col, db.EdgeCalls)
			continue
		}
		// extends/implements: same-file only for now; else park.
		link(srcIdx, e.TargetName, e.Line, e.Col, e.Kind)
	}

	// Pending call/type refs from extractors.
	for _, ref := range refs {
		srcIdx := o.resolveSourceIdx(ref.FromName, ref.FromLine, nodeIdx)
		if srcIdx < 0 {
			srcIdx = 0 // file node
		}
		kind := ref.ReferenceKind
		if kind == "" {
			kind = db.EdgeCalls
		}
		link(srcIdx, ref.ReferenceName, ref.Line, ref.Col, kind)
	}

	// Cross-language bridges: same-file source; target may be foreign placeholder
	// (still written as edge so bridge tooling keeps working; full resolution in step 7).
	bridgeDetector := NewCrossLanguageDetector()
	bridges := bridgeDetector.Detect(source, store, lang)
	for _, bridge := range bridges {
		targetName := strings.TrimSpace(bridge.TargetFunc)
		if targetName == "" {
			continue
		}
		srcIdx := -1
		bestLine := -1
		for _, n := range nodes {
			if n.Kind != "function" && n.Kind != "method" {
				continue
			}
			end := n.EndLine
			if end == 0 {
				end = n.Line
			}
			if n.Line <= bridge.Line && bridge.Line <= end && n.Line >= bestLine {
				if idx, ok := nodeIdx[fmt.Sprintf("%s:%d", n.Name, n.Line)]; ok {
					srcIdx = idx
					bestLine = n.Line
				}
			}
		}
		if srcIdx < 0 {
			srcIdx = 0 // file node
		}
		link(srcIdx, targetName, bridge.Line, 0, "bridge")
	}

	// Single-transaction replace: clear old rows + insert batch + file record.
	fr := db.FileRecord{
		Path:        store,
		ContentHash: contentHash,
		Language:    lang,
		NodeCount:   len(nodes),
	}
	if info, serr := os.Stat(path); serr == nil {
		fr.Size = info.Size()
		fr.Mtime = float64(info.ModTime().UnixMilli())
	} else {
		log.Printf("stat after index %s: %v", path, serr)
	}
	// Re-index deletes this file's nodes and CASCADE-drops inbound edges
	// from other files. Park those edges as pending refs first so
	// ResolveForFiles can rebuild them against the new node ids.
	if perr := o.db.ParkInboundRefsForFile(store); perr != nil {
		return 0, perr
	}
	if _, werr := o.db.ReplaceFileIndex(store, dbNodes, dbEdges, parks, &fr, moduleNodes...); werr != nil {
		return 0, werr
	}

	return len(nodes), nil
}

// extractFile runs the extraction pipeline: tree-sitter preferred, regex
// fallback when tree-sitter fails. Returns the result, whether tree-sitter
// itself errored (false when it succeeded, was unavailable, or regex is the
// only extractor), and an error only when every extractor failed (A4) —
// callers then keep the previous index for the file. One exception: when
// tree-sitter TIMED OUT (ErrTSParseTimeout) and the regex fallback produced
// a non-empty result, the sentinel is returned as the error so callers can
// tell "timeout" from a genuine parse verdict and keep a good old index
// instead of overwriting it with weak regex partial results (must-fix).

// extractFile runs the extraction pipeline: tree-sitter preferred, regex
// fallback when tree-sitter fails. Returns the result, whether tree-sitter
// itself errored (false when it succeeded, was unavailable, or regex is the
// only extractor), and an error only when every extractor failed (A4) —
// callers then keep the previous index for the file. One exception: when
// tree-sitter TIMED OUT (ErrTSParseTimeout) and the regex fallback produced
// a non-empty result, the sentinel is returned as the error so callers can
// tell "timeout" from a genuine parse verdict and keep a good old index
// instead of overwriting it with weak regex partial results (must-fix).
func (o *Orchestrator) extractFile(lang, source, store string) (ExtractResult, bool, error) {
	if o.extractFn != nil {
		return o.extractFn(lang, source, store)
	}
	if ts := NewTreeSitterExtractor(lang); ts != nil {
		res, err := ts.Extract(source, store)
		if err == nil {
			return res, false, nil
		}
		log.Printf("tree-sitter extraction failed for %s (%s), falling back to regex: %v", store, lang, err)
		// S1: remember the tree-sitter failure so the caller can tell an
		// unparseable file (fallback also empty) apart from a successfully
		// extracted empty file, and keep the old index in the former case.
		fb, ferr := NewExtractor(lang).Extract(source, store)
		if errors.Is(err, ErrTSParseTimeout) && ferr == nil {
			// Timeout is NOT a parse verdict: signal it via the sentinel so
			// the caller keeps the old index when one exists (see indexFile).
			return fb, true, fmt.Errorf("%w: %s parse timed out, regex fallback used", ErrTSParseTimeout, lang)
		}
		return fb, true, ferr
	}
	res, err := NewExtractor(lang).Extract(source, store)
	return res, false, err
}

// resolveSourceIdx finds the dbNodes index for a ref/edge source name.
// Returns 0 for the file node when name is empty, -1 when no node matches
// (callers decide whether to skip or fall back to the file node).

// resolveSourceIdx finds the dbNodes index for a ref/edge source name.
// Returns 0 for the file node when name is empty, -1 when no node matches
// (callers decide whether to skip or fall back to the file node).
func (o *Orchestrator) resolveSourceIdx(name string, line int, nodeIdx map[string]int) int {
	if name == "" {
		return 0 // file node
	}
	if line > 0 {
		if idx, ok := nodeIdx[fmt.Sprintf("%s:%d", name, line)]; ok {
			return idx
		}
	}
	if idx, ok := nodeIdx[name]; ok {
		return idx
	}
	// Bare name not found — no better match exists.
	return -1
}

// IndexChanges indexes only files that have changed since last index.
// files are filesystem paths (absolute from watcher/git); resolution uses storage keys.
// A6: paths that no longer exist (deleted / renamed away) drop their old index
// rows instead of being silently skipped. A8: failures are aggregated and
// returned; successfully indexed files keep their partial results.

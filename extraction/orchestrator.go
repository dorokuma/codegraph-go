package extraction

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/codegraph-go/db"
	"github.com/dorokuma/codegraph-go/resolution"
)

// Orchestrator manages the extraction pipeline.
// Step 2 model: extract → write nodes/files → same-file edges now;
// cross-file refs land in unresolved_refs (pending) for step 3 resolution.
type Orchestrator struct {
	db      *db.DB
	workdir string
	force   bool // when true, indexIfNeeded always reindexes
}

// NewOrchestrator creates a new extraction orchestrator.
func NewOrchestrator(database *db.DB, workdir string) *Orchestrator {
	return &Orchestrator{db: database, workdir: workdir}
}

// SetForceReindex makes the next IndexAll/IndexChanges ignore mtime short-circuit.
func (o *Orchestrator) SetForceReindex(v bool) { o.force = v }

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

// parkUnresolved writes a pending unresolved_refs row (cross-file / unknown target).
func (o *Orchestrator) parkUnresolved(fromID int64, refName, kind, file, lang string, line, col int) {
	if fromID == 0 || refName == "" {
		return
	}
	// Pure framework noise with no project symbol is skipped; real symbols
	// (even if named like emit/on) still park so resolve can link them.
	if !ShouldParkRef(o.db, refName) {
		return
	}
	if _, err := o.db.InsertUnresolvedRef(&db.UnresolvedRef{
		FromNode:      fromID,
		ReferenceName: refName,
		ReferenceKind: kind,
		Line:          line,
		Col:           col,
		FilePath:      file,
		Language:      lang,
		Status:        "pending",
		NameTail:      NameTail(refName),
	}); err != nil {
		log.Printf("insert unresolved %s %s: %v", kind, refName, err)
	}
}

// linkSameFileOrPark writes an edge when target is in sameFileIDs; otherwise parks pending.
func (o *Orchestrator) linkSameFileOrPark(sourceID int64, targetName string, sameFileIDs map[string]int64, file, lang string, line, col int, kind string) {
	if sourceID == 0 || targetName == "" {
		return
	}
	if tid, ok := sameFileIDs[targetName]; ok && tid > 0 {
		if _, err := o.db.UpsertEdge(&db.Edge{
			SourceID:   sourceID,
			TargetID:   tid,
			Kind:       kind,
			File:       file,
			Line:       line,
			Col:        col,
			Provenance: "exact",
		}); err != nil {
			log.Printf("upsert same-file %s edge ->%s: %v", kind, targetName, err)
		}
		return
	}
	o.parkUnresolved(sourceID, targetName, kind, file, lang, line, col)
}

// maxIndexFileSize skips oversized blobs (minified bundles, generated dumps).
const maxIndexFileSize = 1 * 1024 * 1024

// visitIndexable walks the workspace once, applying the shared skip rules, and
// invokes fn for each language-supported source file under the size limit.
// Walk errors on individual paths are skipped so one bad path cannot abort the scan.
func (o *Orchestrator) visitIndexable(fn func(path string, info os.FileInfo, lang string) error) error {
	return filepath.Walk(o.workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if ShouldSkipDirIn(o.workdir, path, info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Size() > maxIndexFileSize {
			return nil
		}
		lang := DetectLanguage(path)
		if lang == "" || !IsSupportedLanguage(lang) {
			return nil
		}
		return fn(path, info, lang)
	})
}

// indexIfNeeded reindexes path when the DB says it is stale.
// On success returns (1, nodeCount) even if nodeCount is 0; skip/error returns (0, 0).
func (o *Orchestrator) indexIfNeeded(path string, info os.FileInfo, lang string) (files int, nodes int) {
	if !o.force {
		needsReindex, err := o.db.FileNeedsReindex(path, info.Size(), float64(info.ModTime().Unix()))
		if err != nil || !needsReindex {
			return 0, 0
		}
	}
	nodeCount, err := o.indexFile(path, lang)
	if err != nil {
		log.Printf("index %s: %v", path, err)
		return 0, 0
	}
	return 1, nodeCount
}

// indexWorkerCount picks how many files to extract/index in parallel.
// DB writes are serialized by db.DB's mutex; only CPU-bound extract runs free.
// Override with CODEGRAPH_INDEX_WORKERS (1 = serial rollback; cap 16).
func indexWorkerCount() int {
	if v := strings.TrimSpace(os.Getenv("CODEGRAPH_INDEX_WORKERS")); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			if n < 1 {
				return 1
			}
			if n > 16 {
				return 16
			}
			return n
		}
	}
	n := runtime.NumCPU() - 1
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

type indexJob struct {
	path string
	info os.FileInfo
	lang string
}

// runIndexJobs fans out indexIfNeeded across a small worker pool.
func (o *Orchestrator) runIndexJobs(jobs []indexJob, onEach func(done, total int)) (int, int) {
	if len(jobs) == 0 {
		return 0, 0
	}
	workers := indexWorkerCount()
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers <= 1 {
		totalFiles, totalNodes := 0, 0
		for i, j := range jobs {
			f, n := o.indexIfNeeded(j.path, j.info, j.lang)
			totalFiles += f
			totalNodes += n
			if onEach != nil {
				onEach(i+1, len(jobs))
			}
		}
		return totalFiles, totalNodes
	}

	var (
		mu          sync.Mutex
		totalFiles  int
		totalNodes  int
		done        int
		wg          sync.WaitGroup
		ch          = make(chan indexJob, workers*2)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				func(j indexJob) {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("index worker panic: %v", r)
						}
					}()
					f, n := o.indexIfNeeded(j.path, j.info, j.lang)
					mu.Lock()
					totalFiles += f
					totalNodes += n
					done++
					cur, tot := done, len(jobs)
					mu.Unlock()
					if onEach != nil {
						onEach(cur, tot)
					}
				}(j)
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return totalFiles, totalNodes
}

// collectIndexJobs walks the workspace once into a job list.
func (o *Orchestrator) collectIndexJobs() ([]indexJob, error) {
	var jobs []indexJob
	err := o.visitIndexable(func(path string, info os.FileInfo, lang string) error {
		jobs = append(jobs, indexJob{path: path, info: info, lang: lang})
		return nil
	})
	return jobs, err
}

// IndexAll indexes all files in the workspace (skips unchanged unless force).
func (o *Orchestrator) IndexAll() (int, int, error) {
	jobs, err := o.collectIndexJobs()
	if err != nil {
		return 0, 0, err
	}
	totalFiles, totalNodes := o.runIndexJobs(jobs, func(done, total int) {
		if done%500 == 0 {
			log.Printf("indexed progress %d/%d candidates", done, total)
		}
	})

	// Step 3: turn pending unresolved_refs into cross-file edges.
	if st, rerr := resolution.ResolveAll(o.db, o.workdir); rerr != nil {
		log.Printf("resolve all: %v", rerr)
	} else if st.Resolved > 0 || st.Failed > 0 {
		log.Printf("resolved %d edges (%d failed, %d retried)", st.Resolved, st.Failed, st.Retried)
	}
	// Step 7: dynamic-dispatch synthesis (callback / React / bridge…).
	o.runSynthesis(nil)
	return totalFiles, totalNodes, nil
}

// runSynthesis runs noise scrubbing + dynamic-dispatch synthesis.
// When files is non-nil only refs related to those files are scrubbed;
// nil means scrub all. SynthesizeAll is always full-table (its bottom pass).
func (o *Orchestrator) runSynthesis(files []string) {
	// Drop pure-noise failed/pending refs with no project symbol first.
	o.scrubNoise(files)
	st, err := resolution.SynthesizeAll(o.db, o.workdir)
	if err != nil {
		log.Printf("synthesize: %v", err)
		return
	}
	if st.Written > 0 {
		log.Printf("synthesized %d edges %v", st.Written, st.ByPass)
	}
}

// scrubNoise drops failed/pending pure-noise refs with no matching symbol.
// When files is non-nil, only refs belonging to those files are examined;
// nil means scrub all. hasProjectSymbol results are cached per-call.
func (o *Orchestrator) scrubNoise(files []string) {
	n, err := ScrubNoisyFailedRefsForFiles(o.db, files)
	if err != nil {
		log.Printf("scrub noisy refs: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scrubbed %d noisy unresolved refs", n)
	}
}

// RebuildAll wipes the symbol index and force-reindexes everything.
func (o *Orchestrator) RebuildAll() (int, int, error) {
	if err := o.db.WipeIndex(); err != nil {
		return 0, 0, err
	}
	o.force = true
	defer func() { o.force = false }()
	files, nodes, err := o.IndexAll()
	if err != nil {
		return files, nodes, err
	}
	if err := o.db.SetSchemaRevision(); err != nil {
		log.Printf("set schema revision: %v", err)
	}
	return files, nodes, nil
}

// IndexFile indexes a single file.
func (o *Orchestrator) IndexFile(path string) (int, error) {
	lang := DetectLanguage(path)
	if lang == "" || !IsSupportedLanguage(lang) {
		return 0, fmt.Errorf("unsupported language for %s", path)
	}
	n, err := o.indexFile(path, lang)
	if err != nil {
		return n, err
	}
	if _, rerr := resolution.ResolveForFiles(o.db, o.workdir, []string{path}); rerr != nil {
		log.Printf("resolve after index %s: %v", path, rerr)
	}
	o.runSynthesis([]string{path})
	return n, nil
}

// DeleteFile removes a file from the index.
func (o *Orchestrator) DeleteFile(path string) error {
	return o.db.ClearFile(path)
}

func hashContent(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (o *Orchestrator) indexFile(path string, lang string) (int, error) {
	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	contentHash := hashContent(data)

	// Content-hash short-circuit: mtime can change without edits (touch, checkout).
	// Skip clear+re-extract when the bytes are identical (unless force rebuild).
	if !o.force {
		if same, herr := o.db.FileHasContentHash(path, contentHash); herr == nil && same {
			if info, serr := os.Stat(path); serr == nil {
				// Refresh size/mtime so the cheap FileNeedsReindex stays quiet.
				_ = o.db.TouchFileMeta(path, info.Size(), float64(info.ModTime().Unix()), contentHash)
			}
			return 0, nil
		}
	}

	// Clear old data for this file
	if err := o.db.ClearFile(path); err != nil {
		return 0, err
	}

	// Try tree-sitter extractor first, fall back to regex.
	var result ExtractResult
	tsExt := NewTreeSitterExtractor(lang)
	if tsExt != nil {
		result = tsExt.Extract(string(data), path)
	} else {
		result = NewExtractor(lang).Extract(string(data), path)
	}
	nodes := result.Nodes
	edges := result.Edges
	refs := result.Refs

	// Detect framework routes (linked to handlers after node insert).
	detector := NewFrameworkDetector()
	routes := detector.DetectRoutes(string(data), path, lang)
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

	// Create a file-level node to anchor import/bridge edges.
	fileNodeID, err := o.db.UpsertNode(&db.Node{
		Kind:     db.KindFile,
		Name:     path,
		File:     path,
		Line:     0,
		Language: lang,
	})
	if err != nil {
		log.Printf("upsert file node %s: %v", path, err)
	}

	// Insert nodes
	nodeIDMap := make(map[string]int64) // "name:line" and bare name → id
	type bareHit struct {
		id   int64
		kind string
		line int
	}
	bareBest := map[string]bareHit{}
	for _, n := range nodes {
		id, err := o.db.UpsertNode(&db.Node{
			Kind:          n.Kind,
			Name:          n.Name,
			File:          path,
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
		if err != nil {
			log.Printf("upsert node %s: %v", n.Name, err)
			continue
		}
		key := fmt.Sprintf("%s:%d", n.Name, n.Line)
		nodeIDMap[key] = id
		prev, ok := bareBest[n.Name]
		rank := bareRank(n.Kind)
		if !ok || rank > bareRank(prev.kind) || (rank == bareRank(prev.kind) && n.Line < prev.line) {
			bareBest[n.Name] = bareHit{id: id, kind: n.Kind, line: n.Line}
		}
	}
	sameFileBare := make(map[string]int64, len(bareBest))
	for name, hit := range bareBest {
		nodeIDMap[name] = hit.id
		sameFileBare[name] = hit.id
	}

	// Route → handler: same-file edge now; cross-file → unresolved_refs.
	for _, route := range routes {
		handler := simplifyHandlerName(route.Handler)
		if handler == "" {
			continue
		}
		routeKey := fmt.Sprintf("%s:%d", route.Method+" "+route.Path, route.Line)
		routeID, ok := nodeIDMap[routeKey]
		if !ok {
			continue
		}
		o.linkSameFileOrPark(routeID, handler, sameFileBare, path, lang, route.Line, 0, db.EdgeReferences)
	}

	// Structural edges (imports, etc.)
	for _, e := range edges {
		if e.Kind == "imports" {
			targetNodes, err := o.db.GetNodeByName(e.TargetName)
			if err != nil {
				log.Printf("lookup import target %s: %v", e.TargetName, err)
			}
			var targetID int64
			if len(targetNodes) > 0 {
				targetID = targetNodes[0].ID
			} else {
				targetID, err = o.db.UpsertNode(&db.Node{
					Kind:     "module",
					Name:     e.TargetName,
					File:     e.TargetName,
					Line:     0,
					Language: lang,
				})
				if err != nil {
					log.Printf("upsert import module %s: %v", e.TargetName, err)
				}
			}
			if targetID > 0 && fileNodeID > 0 {
				if _, err := o.db.UpsertEdge(&db.Edge{
					SourceID:   fileNodeID,
					TargetID:   targetID,
					Kind:       "imports",
					File:       path,
					Line:       e.Line,
					Provenance: "exact",
				}); err != nil {
					log.Printf("upsert import edge %s: %v", e.TargetName, err)
				}
			}
			continue
		}

		// Any leftover non-import edges (extends/implements/calls not promoted).
		sourceID := o.resolveSourceID(e.SourceName, e.Line, nodeIDMap, fileNodeID)
		if sourceID == 0 {
			continue
		}
		if e.Kind == "calls" {
			o.linkSameFileOrPark(sourceID, e.TargetName, sameFileBare, path, lang, e.Line, e.Col, db.EdgeCalls)
			continue
		}
		// extends/implements: same-file only for now; else park.
		o.linkSameFileOrPark(sourceID, e.TargetName, sameFileBare, path, lang, e.Line, e.Col, e.Kind)
	}

	// Pending call/type refs from extractors.
	for _, ref := range refs {
		sourceID := o.resolveSourceID(ref.FromName, ref.FromLine, nodeIDMap, fileNodeID)
		if sourceID == 0 {
			sourceID = fileNodeID
		}
		kind := ref.ReferenceKind
		if kind == "" {
			kind = db.EdgeCalls
		}
		o.linkSameFileOrPark(sourceID, ref.ReferenceName, sameFileBare, path, lang, ref.Line, ref.Col, kind)
	}

	// Cross-language bridges: same-file source; target may be foreign placeholder
	// (still written as edge so bridge tooling keeps working; full resolution in step 7).
	bridgeDetector := NewCrossLanguageDetector()
	bridges := bridgeDetector.Detect(string(data), path, lang)
	for _, bridge := range bridges {
		targetName := strings.TrimSpace(bridge.TargetFunc)
		if targetName == "" {
			continue
		}
		sourceID := int64(0)
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
				if id, ok := nodeIDMap[fmt.Sprintf("%s:%d", n.Name, n.Line)]; ok {
					sourceID = id
					bestLine = n.Line
				}
			}
		}
		if sourceID == 0 {
			sourceID = fileNodeID
		}
		// Prefer same-file target; else park as unresolved (no more heuristic cross-file).
		if tid, ok := sameFileBare[targetName]; ok && tid > 0 {
			if _, err := o.db.UpsertEdge(&db.Edge{
				SourceID:   sourceID,
				TargetID:   tid,
				Kind:       "bridge",
				File:       path,
				Line:       bridge.Line,
				Provenance: "exact",
			}); err != nil {
				log.Printf("upsert bridge edge %s: %v", targetName, err)
			}
		} else {
			o.parkUnresolved(sourceID, targetName, "bridge", path, lang, bridge.Line, 0)
		}
	}

	// Record file (+ language / node count / content_hash for schema v7 fields).
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("stat after index %s: %v", path, err)
	} else {
		if err := o.db.UpsertFileRecord(&db.FileRecord{
			Path:        path,
			Size:        info.Size(),
			Mtime:       float64(info.ModTime().Unix()),
			ContentHash: contentHash,
			Language:    lang,
			NodeCount:   len(nodes),
		}); err != nil {
			log.Printf("upsert file record %s: %v", path, err)
		}
	}

	return len(nodes), nil
}

// resolveSourceID finds the enclosing symbol id for a ref/edge source name.
func (o *Orchestrator) resolveSourceID(name string, line int, nodeIDMap map[string]int64, fileNodeID int64) int64 {
	if name == "" {
		return fileNodeID
	}
	if line > 0 {
		if id, ok := nodeIDMap[fmt.Sprintf("%s:%d", name, line)]; ok {
			return id
		}
	}
	if id, ok := nodeIDMap[name]; ok {
		return id
	}
	// Bare name not found — no need to scan nodeIDMap linearly;
	// splitNameLineKey would not find a better match here.
	return 0
}

// IndexChanges indexes only files that have changed since last index.
func (o *Orchestrator) IndexChanges(files []string) (int, int, error) {
	totalFiles := 0
	totalNodes := 0

	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() || info.Size() > maxIndexFileSize {
			continue
		}
		lang := DetectLanguage(path)
		if lang == "" || !IsSupportedLanguage(lang) {
			continue
		}
		filesN, nodes := o.indexIfNeeded(path, info, lang)
		totalFiles += filesN
		totalNodes += nodes
	}

	if _, err := resolution.ResolveForFiles(o.db, o.workdir, files); err != nil {
		log.Printf("resolve changes: %v", err)
	}
	o.runSynthesis(files)
	return totalFiles, totalNodes, nil
}

// ProgressFunc is called during indexing to report progress.
type ProgressFunc func(phase string, current, total int)

// IndexAllWithProgress indexes all files with progress reporting.
// Same walk/skip/index path as IndexAll (collect jobs, then parallel pool).
//
// Thread safety: onProgress may be called concurrently by multiple worker
// goroutines. Callers must ensure their implementation is thread-safe.
func (o *Orchestrator) IndexAllWithProgress(onProgress ProgressFunc) (int, int, error) {
	start := time.Now()
	jobs, err := o.collectIndexJobs()
	if err != nil {
		return 0, 0, err
	}
	totalCandidates := len(jobs)
	indexedFiles, indexedNodes := o.runIndexJobs(jobs, func(done, total int) {
		if onProgress != nil && (done%10 == 0 || done == total) {
			onProgress("indexing", done, totalCandidates)
		}
	})

	if st, rerr := resolution.ResolveAll(o.db, o.workdir); rerr != nil {
		log.Printf("resolve all: %v", rerr)
	} else if st.Resolved > 0 {
		log.Printf("resolved %d edges after index", st.Resolved)
	}
	o.runSynthesis(nil)
	elapsed := time.Since(start)
	pending, _ := o.db.CountUnresolvedRefs("pending")
	log.Printf("indexing complete: %d files, %d nodes, %d pending refs in %v (workers=%d)",
		indexedFiles, indexedNodes, pending, elapsed, indexWorkerCount())

	return indexedFiles, indexedNodes, nil
}

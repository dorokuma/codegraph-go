package extraction

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/dorokuma/codegraph-go/db"
)

// Orchestrator manages the extraction pipeline.
type Orchestrator struct {
	db      *db.DB
	workdir string
}

// NewOrchestrator creates a new extraction orchestrator.
func NewOrchestrator(database *db.DB, workdir string) *Orchestrator {
	return &Orchestrator{db: database, workdir: workdir}
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
			if ShouldSkipDir(path, info.Name()) {
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
	needsReindex, err := o.db.FileNeedsReindex(path, info.Size(), float64(info.ModTime().Unix()))
	if err != nil || !needsReindex {
		return 0, 0
	}
	nodeCount, err := o.indexFile(path, lang)
	if err != nil {
		log.Printf("index %s: %v", path, err)
		return 0, 0
	}
	return 1, nodeCount
}

// IndexAll indexes all files in the workspace.
func (o *Orchestrator) IndexAll() (int, int, error) {
	totalFiles := 0
	totalNodes := 0

	err := o.visitIndexable(func(path string, info os.FileInfo, lang string) error {
		files, nodes := o.indexIfNeeded(path, info, lang)
		if files == 0 {
			return nil
		}
		totalFiles += files
		totalNodes += nodes
		if totalFiles%500 == 0 {
			log.Printf("indexed %d files, %d nodes", totalFiles, totalNodes)
		}
		return nil
	})

	return totalFiles, totalNodes, err
}

// IndexFile indexes a single file.
func (o *Orchestrator) IndexFile(path string) (int, error) {
	lang := DetectLanguage(path)
	if lang == "" || !IsSupportedLanguage(lang) {
		return 0, fmt.Errorf("unsupported language for %s", path)
	}
	return o.indexFile(path, lang)
}

// DeleteFile removes a file from the index.
func (o *Orchestrator) DeleteFile(path string) error {
	return o.db.ClearFile(path)
}

func (o *Orchestrator) indexFile(path string, lang string) (int, error) {
	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	// Clear old data for this file
	if err := o.db.ClearFile(path); err != nil {
		return 0, err
	}

	// Try tree-sitter extractor first, fall back to regex
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	tsExt := NewTreeSitterExtractor(lang)
	if tsExt != nil {
		nodes, edges = tsExt.Extract(string(data), path)
	} else {
		ext := NewExtractor(lang)
		nodes, edges = ext.Extract(string(data), path)
	}

	// Detect framework routes
	detector := NewFrameworkDetector()
	routes := detector.DetectRoutes(string(data), path, lang)
	for _, route := range routes {
		nodes = append(nodes, ExtractedNode{
			Kind:     "route",
			Name:     route.Method + " " + route.Path,
			File:     route.File,
			Line:     route.Line,
			EndLine:  route.Line,
			Body:     route.Handler,
			Language: lang,
		})
	}

	// Create a file-level node to anchor import/bridge edges.
	// Previously source_id=0 was used, which caused UNIQUE conflicts
	// (multiple files importing the same package) and orphaned records
	// (ClearFile could not find edges with source_id=0).
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
	nodeIDMap := make(map[string]int64)
	for _, n := range nodes {
		id, err := o.db.UpsertNode(&db.Node{
			Kind:     n.Kind,
			Name:     n.Name,
			File:     path,
			Line:     n.Line,
			EndLine:  n.EndLine,
			Body:     n.Body,
			Language: lang,
		})
		if err != nil {
			log.Printf("upsert node %s: %v", n.Name, err)
			continue
		}
		key := fmt.Sprintf("%s:%d", n.Name, n.Line)
		nodeIDMap[key] = id
	}

	// Insert edges
	for _, e := range edges {
		if e.Kind == "imports" {
			// Import edges: store as file-level edge
			// We create a placeholder node for the import target if it doesn't exist
			targetNodes, err := o.db.GetNodeByName(e.TargetName)
			if err != nil {
				log.Printf("lookup import target %s: %v", e.TargetName, err)
			}
			var targetID int64
			if len(targetNodes) > 0 {
				targetID = targetNodes[0].ID
			} else {
				// Create a placeholder node for the import target
				targetID, err = o.db.UpsertNode(&db.Node{
					Kind:     "module",
					Name:     e.TargetName,
					File:     e.TargetName, // Use import path as file
					Line:     0,
					Language: lang,
				})
				if err != nil {
					log.Printf("upsert import module %s: %v", e.TargetName, err)
				}
			}
			if targetID > 0 && fileNodeID > 0 {
				if _, err := o.db.UpsertEdge(&db.Edge{
					SourceID: fileNodeID,
					TargetID: targetID,
					Kind:     "imports",
					File:     path,
					Line:     e.Line,
				}); err != nil {
					log.Printf("upsert import edge %s: %v", e.TargetName, err)
				}
			}
			continue
		}

		// Find source and target node IDs
		sourceKey := fmt.Sprintf("%s:%d", e.SourceName, e.Line)
		sourceID, sourceOk := nodeIDMap[sourceKey]

		// For calls, try to find target in the same file first
		var targetID int64
		if e.Kind == "calls" {
			// Try to find target node
			targetNodes, err := o.db.GetNodeByName(e.TargetName)
			if err != nil {
				log.Printf("lookup call target %s: %v", e.TargetName, err)
			}
			if len(targetNodes) > 0 {
				targetID = targetNodes[0].ID
			}
		}

		if sourceOk && targetID > 0 {
			if _, err := o.db.UpsertEdge(&db.Edge{
				SourceID: sourceID,
				TargetID: targetID,
				Kind:     e.Kind,
				File:     path,
				Line:     e.Line,
			}); err != nil {
				log.Printf("upsert call edge %s->%s: %v", e.SourceName, e.TargetName, err)
			}
		}
	}

	// Detect cross-language bridges
	bridgeDetector := NewCrossLanguageDetector()
	bridges := bridgeDetector.Detect(string(data), path, lang)
	for _, bridge := range bridges {
		// Create or find target node for the bridge
		var targetID int64
		targetNodes, err := o.db.GetNodeByName(bridge.TargetFunc)
		if err != nil {
			log.Printf("lookup bridge target %s: %v", bridge.TargetFunc, err)
		}
		if len(targetNodes) > 0 {
			targetID = targetNodes[0].ID
		} else {
			// Create a placeholder node for the foreign function
			targetID, err = o.db.UpsertNode(&db.Node{
				Kind:     "foreign_function",
				Name:     bridge.TargetFunc,
				File:     path,
				Line:     bridge.Line,
				Language: bridge.TargetLang,
			})
			if err != nil {
				log.Printf("upsert foreign function %s: %v", bridge.TargetFunc, err)
			}
		}

		// Create bridge edge
		if targetID > 0 && fileNodeID > 0 {
			if _, err := o.db.UpsertEdge(&db.Edge{
				SourceID: fileNodeID,
				TargetID: targetID,
				Kind:     "bridge",
				File:     path,
				Line:     bridge.Line,
			}); err != nil {
				log.Printf("upsert bridge edge %s: %v", bridge.TargetFunc, err)
			}
		}
	}

	// Record file
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("stat after index %s: %v", path, err)
	} else {
		if err := o.db.UpsertFile(path, info.Size(), float64(info.ModTime().Unix())); err != nil {
			log.Printf("upsert file record %s: %v", path, err)
		}
	}

	return len(nodes), nil
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

	return totalFiles, totalNodes, nil
}

// ProgressFunc is called during indexing to report progress.
type ProgressFunc func(phase string, current, total int)

// IndexAllWithProgress indexes all files with progress reporting.
// Uses the same walk/skip/index path as IndexAll (two passes: count then index).
func (o *Orchestrator) IndexAllWithProgress(onProgress ProgressFunc) (int, int, error) {
	totalCandidates := 0
	_ = o.visitIndexable(func(path string, info os.FileInfo, lang string) error {
		totalCandidates++
		return nil
	})

	current := 0
	indexedFiles := 0
	indexedNodes := 0
	start := time.Now()

	err := o.visitIndexable(func(path string, info os.FileInfo, lang string) error {
		current++
		if onProgress != nil && current%10 == 0 {
			onProgress("indexing", current, totalCandidates)
		}
		files, nodes := o.indexIfNeeded(path, info, lang)
		indexedFiles += files
		indexedNodes += nodes
		return nil
	})

	elapsed := time.Since(start)
	log.Printf("indexing complete: %d files, %d nodes in %v", indexedFiles, indexedNodes, elapsed)

	return indexedFiles, indexedNodes, err
}

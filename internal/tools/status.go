package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
)

// StatusArgs are the arguments for the status tool.
type StatusArgs struct {
	Path string `json:"path,omitempty" jsonschema:"optional path to check specific file status,optional"`
}

// StatusResult is the result of the status tool.
type StatusResult struct {
	Content []ContentItem `json:"content"`
}

// ContentItem represents a text content item in MCP response.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolStatus returns index health and statistics.
// DB reads now accept context via Context variants; cancellation is supported.
// workdirs is the full list of workspace roots (for broad-workdir detection);
// workdir is the specific project root for this call.
// dropped is the watcher's permanent-drop count (paths discarded after the
// sync retry budget was exhausted); surfaced here so silent index gaps are
// observable (see Watcher.DroppedCount).
func ToolStatus(ctx context.Context, database *db.DB, workdirs []string, workdir string, args StatusArgs, pendingFiles []string, dropped uint64) (*StatusResult, error) {
	stats, err := database.GetStatsContext(ctx)
	if err != nil {
		return nil, err
	}

	var b strings.Builder

	b.WriteString(fmt.Sprintf("Nodes: %d · Edges: %d · Files: %d\n", stats.NodeCount, stats.EdgeCount, stats.FileCount))

	if len(stats.KindCounts) > 0 {
		parts := make([]string, 0, len(stats.KindCounts))
		for kind, count := range stats.KindCounts {
			parts = append(parts, fmt.Sprintf("%s:%d", kind, count))
		}
		b.WriteString(strings.Join(parts, ", "))
		b.WriteByte('\n')
	}

	// Show the DB path relative to the workdir — never the absolute path,
	// which would leak host directory layout into agent-visible output
	// (audit medium). Falls back to the basename when the DB lives outside
	// the workdir.
	dbPath := database.Path()
	if rel := db.RelPath(workdir, dbPath); rel != "" && rel != dbPath && !filepath.IsAbs(rel) {
		dbPath = rel
	} else {
		dbPath = filepath.Base(dbPath)
	}
	b.WriteString(fmt.Sprintf("DB: %s (schema=%s)\n", dbPath, db.SchemaRevision()))
	need, old, rebuildErr := database.NeedsRebuildContext(ctx)
	if rebuildErr != nil {
		b.WriteString(fmt.Sprintf("Rebuild check failed: %v\n", rebuildErr))
	} else if need {
		b.WriteString(fmt.Sprintf("Rebuild pending: %s → %s\n", old, db.SchemaRevision()))
	}

	if len(pendingFiles) > 0 {
		b.WriteString(fmt.Sprintf("Pending: %d files\n", len(pendingFiles)))
	}
	if dropped > 0 {
		b.WriteString(fmt.Sprintf("Dropped: %d files (sync retries exhausted; touch or re-index to re-enqueue)\n", dropped))
	}

	// Home-mode: list which projects are indexed under any workdir.
	anyBroad := false
	for _, wd := range workdirs {
		if extraction.IsBroadWorkdir(wd) {
			anyBroad = true
			break
		}
	}
	if anyBroad {
		b.WriteString("\nIndexed projects:\n")
		found := 0
		for _, wd := range workdirs {
			entries, readErr := os.ReadDir(wd)
			if readErr != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				full := filepath.Join(wd, e.Name())
				if extraction.ShouldSkipDirIn(wd, full, e.Name()) {
					continue
				}
				if !extraction.HasProjectMarker(full) {
					continue
				}
				b.WriteString(fmt.Sprintf("- %s/\n", e.Name()))
				found++
			}
		}
		if found == 0 {
			b.WriteString("(no project markers found)\n")
		}
	}

	if args.Path != "" {
		files, listErr := database.ListFilesContext(ctx)
		if listErr != nil {
			b.WriteString(fmt.Sprintf("ListFiles error: %v\n", listErr))
		}
		found := false
		// Workdir self-reference: in per-root mode (workdir IS the project
		// directory) the files table stores workdir-relative keys
		// (cmd/…, internal/…) that never contain the workdir's own name, so
		// a query for the project itself — its basename, the workdir path, or
		// "." — matched nothing and misreported "not indexed". When the
		// query resolves to the workdir itself, any non-empty index counts as
		// indexed: the whole DB belongs to this workdir. In home/main-lib
		// mode (workdir = parent) this branch does not fire and the prefix
		// match below keeps handling project-level queries like "codegraph-go".
		wdClean := filepath.Clean(workdir)
		queryTarget := args.Path
		if !filepath.IsAbs(queryTarget) {
			queryTarget = filepath.Join(wdClean, queryTarget)
		}
		queryTarget = filepath.Clean(queryTarget)
		if queryTarget == wdClean || filepath.Clean(args.Path) == filepath.Base(wdClean) {
			if len(files) > 0 {
				b.WriteString(fmt.Sprintf("%s: indexed\n", args.Path))
				found = true
			}
		}
		if !found {
			// Normalize: try exact, suffix, and prefix (for project-level queries like "codegraph-go")
			norm := filepath.Clean(args.Path)
			if !strings.HasSuffix(norm, string(filepath.Separator)) {
				norm += string(filepath.Separator)
			}
			for _, f := range files {
				if f == args.Path || strings.HasSuffix(f, args.Path) || strings.HasPrefix(f, norm) {
					b.WriteString(fmt.Sprintf("%s: indexed\n", args.Path))
					found = true
					break
				}
			}
		}
		if !found {
			b.WriteString(fmt.Sprintf("%s: not indexed\n", args.Path))
		}
	}

	return &StatusResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

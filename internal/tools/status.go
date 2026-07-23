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
func ToolStatus(ctx context.Context, database *db.DB, workdirs []string, workdir string, args StatusArgs, pendingFiles []string) (*StatusResult, error) {
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

	b.WriteString(fmt.Sprintf("DB: %s (schema=%s)\n", database.Path(), db.SchemaRevision()))
	need, old, rebuildErr := database.NeedsRebuildContext(ctx)
	if rebuildErr != nil {
		b.WriteString(fmt.Sprintf("Rebuild check failed: %v\n", rebuildErr))
	} else if need {
		b.WriteString(fmt.Sprintf("Rebuild pending: %s → %s\n", old, db.SchemaRevision()))
	}

	if len(pendingFiles) > 0 {
		b.WriteString(fmt.Sprintf("Pending: %d files\n", len(pendingFiles)))
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
		if !found {
			b.WriteString(fmt.Sprintf("%s: not indexed\n", args.Path))
		}
	}

	return &StatusResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

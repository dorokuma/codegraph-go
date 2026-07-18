package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
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
func ToolStatus(ctx context.Context, database *db.DB, workdir string, args StatusArgs, pendingFiles []string) (*StatusResult, error) {
	stats, err := database.GetStats()
	if err != nil {
		return nil, err
	}

	var b strings.Builder

	b.WriteString("# CodeGraph Status\n\n")

	// Index stats
	b.WriteString("## Index Statistics\n")
	b.WriteString(fmt.Sprintf("- **Nodes:** %d\n", stats.NodeCount))
	b.WriteString(fmt.Sprintf("- **Edges:** %d\n", stats.EdgeCount))
	b.WriteString(fmt.Sprintf("- **Files:** %d\n", stats.FileCount))

	if len(stats.KindCounts) > 0 {
		b.WriteString("\n### By Kind:\n")
		for kind, count := range stats.KindCounts {
			b.WriteString(fmt.Sprintf("- %s: %d\n", kind, count))
		}
	}

	// Database location
	b.WriteString(fmt.Sprintf("\n## Database\n"))
	b.WriteString(fmt.Sprintf("- Path: %s\n", database.Path()))

	// Pending sync
	if len(pendingFiles) > 0 {
		b.WriteString("\n### Pending sync:\n")
		for _, f := range pendingFiles {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
	} else {
		b.WriteString("\n### Sync Status\n")
		b.WriteString("- No pending files\n")
	}

	// File status check
	if args.Path != "" {
		b.WriteString(fmt.Sprintf("\n## File: %s\n", args.Path))
		files, _ := database.ListFiles()
		found := false
		for _, f := range files {
			if f == args.Path || strings.HasSuffix(f, args.Path) {
				b.WriteString("- Status: indexed\n")
				found = true
				break
			}
		}
		if !found {
			b.WriteString("- Status: not indexed\n")
		}
	}

	return &StatusResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

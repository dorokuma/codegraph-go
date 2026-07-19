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

	b.WriteString(fmt.Sprintf("Nodes: %d · Edges: %d · Files: %d\n", stats.NodeCount, stats.EdgeCount, stats.FileCount))

	if len(stats.KindCounts) > 0 {
		parts := make([]string, 0, len(stats.KindCounts))
		for kind, count := range stats.KindCounts {
			parts = append(parts, fmt.Sprintf("%s:%d", kind, count))
		}
		b.WriteString(strings.Join(parts, ", "))
		b.WriteByte('\n')
	}

	b.WriteString(fmt.Sprintf("DB: %s (logic=%s)\n", database.Path(), db.LogicVersion()))
	if need, old, err := database.NeedsRebuild(); err == nil && need {
		b.WriteString(fmt.Sprintf("Rebuild pending: %s → %s\n", old, db.LogicVersion()))
	}

	if len(pendingFiles) > 0 {
		b.WriteString(fmt.Sprintf("Pending: %d files\n", len(pendingFiles)))
	}

	if args.Path != "" {
		files, _ := database.ListFiles()
		found := false
		for _, f := range files {
			if f == args.Path || strings.HasSuffix(f, args.Path) {
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

package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// NodeArgs are the arguments for the node tool.
type NodeArgs struct {
	Name string `json:"name" jsonschema:"symbol name to look for"`
	File string `json:"file,omitempty" jsonschema:"optional file path to narrow search,optional"`
	Line int    `json:"line,omitempty" jsonschema:"optional line number to find exact symbol,optional"`
}

// NodeResult is the result of the node tool.
type NodeResult struct {
	Content []ContentItem `json:"content"`
}

// ToolNode returns detailed information about a symbol.
func ToolNode(ctx context.Context, database *db.DB, args NodeArgs) (*NodeResult, error) {
	if args.Name == "" && args.File == "" {
		return nil, fmt.Errorf("name or file is required")
	}

	var nodes []db.Node
	var err error

	// If file+line specified, find exact node
	if args.File != "" && args.Line > 0 {
		node, err := database.GetNodeByFileLine(args.File, args.Line)
		if err != nil {
			return nil, err
		}
		if node != nil {
			nodes = []db.Node{*node}
		}
	} else if args.Name != "" {
		// Search by name
		nodes, err = database.GetNodeByName(args.Name)
		if err != nil {
			return nil, err
		}
	}

	if len(nodes) == 0 {
		return &NodeResult{
			Content: []ContentItem{{Type: "text", Text: "no symbols found"}},
		}, nil
	}

	// Build response
	var b strings.Builder
	for i, node := range nodes {
		if i > 0 {
			b.WriteString("\n---\n\n")
		}

		b.WriteString(fmt.Sprintf("# %s (%s)\n", node.Name, node.Kind))
		b.WriteString(fmt.Sprintf("File: %s:%d\n", node.File, node.Line))
		if node.Language != "" {
			b.WriteString(fmt.Sprintf("Language: %s\n", node.Language))
		}

		// Get callers
		callers, _ := database.GetCallers(node.ID)
		if len(callers) > 0 {
			b.WriteString("\n## Called by:\n")
			for _, c := range callers {
				b.WriteString(fmt.Sprintf("- %s (%s) at %s:%d\n", c.Name, c.Kind, c.File, c.Line))
			}
		}

		// Get callees
		callees, _ := database.GetCallees(node.ID)
		if len(callees) > 0 {
			b.WriteString("\n## Calls:\n")
			for _, c := range callees {
				b.WriteString(fmt.Sprintf("- %s (%s) at %s:%d\n", c.Name, c.Kind, c.File, c.Line))
			}
		}

		// Show source body if available
		if node.Body != "" {
			b.WriteString("\n## Source:\n```")
			if node.Language != "" {
				b.WriteString(node.Language)
			}
			b.WriteString("\n")
			b.WriteString(node.Body)
			b.WriteString("\n```\n")
		}
	}

	return &NodeResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

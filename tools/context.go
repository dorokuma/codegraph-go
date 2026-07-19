package tools

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
	"github.com/dorokuma/codegraph-go/extraction"
)

// ContextFormatter formats code graph results as markdown optimized for LLM consumption.
// Ported from official context/formatter.ts concepts.

// ContextNode is a node with its relationship info for formatting.
type ContextNode struct {
	Node     db.Node
	Relation string // "entry", "caller", "callee", "related"
}

// FormatContextAsMarkdown formats a set of nodes and their relationships as markdown.
func FormatContextAsMarkdown(workdir string, query string, nodes []ContextNode) string {
	var b strings.Builder

	b.WriteString("## Code Context\n\n")
	b.WriteString(fmt.Sprintf("**Query:** %s\n\n", query))

	// Separate entry points from related symbols
	var entries, related []ContextNode
	for _, cn := range nodes {
		if cn.Relation == "entry" {
			entries = append(entries, cn)
		} else {
			related = append(related, cn)
		}
	}

	// Sort entries: non-generated first
	sort.SliceStable(entries, func(i, j int) bool {
		aGen := extraction.IsGeneratedFile(entries[i].Node.File)
		bGen := extraction.IsGeneratedFile(entries[j].Node.File)
		if aGen != bGen {
			return !aGen
		}
		return entries[i].Node.Name < entries[j].Node.Name
	})

	// Entry Points section
	if len(entries) > 0 {
		b.WriteString("### Entry Points\n\n")
		for _, cn := range entries {
			n := cn.Node
			rel := db.RelPath(workdir, n.File)
			loc := ""
			if n.Line > 0 {
				loc = fmt.Sprintf(":%d", n.Line)
			}
			b.WriteString(fmt.Sprintf("- **%s** (%s) — %s%s\n", n.Name, n.Kind, rel, loc))
			if n.Signature != "" {
				b.WriteString(fmt.Sprintf("  `%s`\n", n.Signature))
			}
		}
		b.WriteString("\n")
	}

	// Related Symbols section
	if len(related) > 0 {
		// Group by file, limit to 10
		byFile := map[string][]db.Node{}
		for _, cn := range related {
			if !extraction.IsGeneratedFile(cn.Node.File) {
				byFile[cn.Node.File] = append(byFile[cn.Node.File], cn.Node)
			}
		}

		if len(byFile) > 0 {
			b.WriteString("### Related Symbols\n\n")
			count := 0
			for file, fileNodes := range byFile {
				if count >= 10 {
					break
				}
				rel := db.RelPath(workdir, file)
				var parts []string
				for _, n := range fileNodes {
					loc := ""
					if n.Line > 0 {
						loc = fmt.Sprintf(":%d", n.Line)
					}
					parts = append(parts, fmt.Sprintf("%s%s", n.Name, loc))
				}
				b.WriteString(fmt.Sprintf("- %s: %s\n", rel, strings.Join(parts, ", ")))
				count++
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// FormatNodeDetail formats a single node with its callers and callees.
func FormatNodeDetail(workdir string, node *db.Node, callers, callees []db.NodeRef) string {
	var b strings.Builder

	rel := db.RelPath(workdir, node.File)
	loc := fmt.Sprintf("%s:%d", rel, node.Line)
	b.WriteString(fmt.Sprintf("# %s (%s) at %s\n", node.Name, node.Kind, loc))

	if node.Signature != "" {
		b.WriteString(fmt.Sprintf("Signature: `%s`\n", node.Signature))
	}
	if node.ReturnType != "" {
		b.WriteString(fmt.Sprintf("Returns: %s\n", node.ReturnType))
	}
	if node.Visibility != "" {
		b.WriteString(fmt.Sprintf("Visibility: %s\n", node.Visibility))
	}
	b.WriteString("\n")

	// Callers
	if len(callers) > 0 {
		b.WriteString(fmt.Sprintf("## Callers (%d)\n", len(callers)))
		for _, c := range callers {
			cRel := db.RelPath(workdir, c.File)
			via := c.EdgeKind
			if via == "" || via == db.EdgeCalls {
				via = "calls"
			}
			b.WriteString(fmt.Sprintf("- %s (%s) at %s:%d [%s]\n", c.Name, c.Kind, cRel, c.Line, via))
		}
		b.WriteString("\n")
	}

	// Callees
	if len(callees) > 0 {
		b.WriteString(fmt.Sprintf("## Callees (%d)\n", len(callees)))
		for _, c := range callees {
			cRel := db.RelPath(workdir, c.File)
			via := c.EdgeKind
			if via == "" || via == db.EdgeCalls {
				via = "calls"
			}
			b.WriteString(fmt.Sprintf("- %s (%s) at %s:%d [%s]\n", c.Name, c.Kind, cRel, c.Line, via))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// FormatSearchResults formats search results as compact markdown.
func FormatSearchResults(workdir string, query string, nodes []db.Node, limit int) string {
	var b strings.Builder

	if limit <= 0 {
		limit = 50
	}

	// Separate generated from hand-written
	var generated, handwritten []db.Node
	for _, n := range nodes {
		if extraction.IsGeneratedFile(n.File) {
			generated = append(generated, n)
		} else {
			handwritten = append(handwritten, n)
		}
	}

	// Show hand-written first, then generated
	ordered := append(handwritten, generated...)
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}

	b.WriteString(fmt.Sprintf("# Search: %q (%d hits", query, len(nodes)))
	if len(generated) > 0 {
		b.WriteString(fmt.Sprintf(", %d generated", len(generated)))
	}
	b.WriteString(")\n\n")

	for _, n := range ordered {
		rel := db.RelPath(workdir, n.File)
		loc := ""
		if n.Line > 0 {
			loc = fmt.Sprintf(":%d", n.Line)
		}
		gen := ""
		if extraction.IsGeneratedFile(n.File) {
			gen = " [generated]"
		}
		sig := ""
		if n.Signature != "" {
			sig = fmt.Sprintf(" `%s`", n.Signature)
		}
		b.WriteString(fmt.Sprintf("- %s (%s) at %s%s%s%s\n", n.Name, n.Kind, rel, loc, gen, sig))
	}

	return b.String()
}

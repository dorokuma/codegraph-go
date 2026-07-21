package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// NodeArgs are the arguments for the node tool (dual mode, official-aligned).
type NodeArgs struct {
	Name         string `json:"name,omitempty" jsonschema:"symbol name (symbol mode). Omit and pass file alone to read a whole file like Read.,optional"`
	File         string `json:"file,omitempty" jsonschema:"file path or basename. Alone = file-read mode; with name = disambiguate overload.,optional"`
	Line         int    `json:"line,omitempty" jsonschema:"symbol mode: pin definition at/around this line,optional"`
	IncludeCode *bool `json:"includeCode,omitempty" jsonschema:"symbol mode: include body (default false). File mode always returns source unless symbolsOnly.,optional"`
	SymbolsOnly bool  `json:"symbolsOnly,omitempty" jsonschema:"file mode: return symbol map + dependents only, no source,optional"`
	Offset      int   `json:"offset,omitempty" jsonschema:"file mode: 1-based start line (like Read),optional"`
	Limit       int   `json:"limit,omitempty" jsonschema:"file mode: max lines to return (default whole file, cap 2000),optional"`
}

// NodeResult is the result of the node tool.
type NodeResult struct {
	Content []ContentItem `json:"content"`
	// FileMode is true when this was a whole-file read (higher MCP output budget).
	FileMode bool `json:"-"`
}

const (
	nodeBodyBudget   = 12000
	nodeHardCap      = 16
	nodeTrailCap     = 12
	nodeFileCharBudg = 38000
	nodeFileLineDef  = 2000
)

// ToolNode returns detailed information about a symbol or file.
func ToolNode(ctx context.Context, database *db.DB, args NodeArgs) (*NodeResult, error) {
	return ToolNodeIn(ctx, database, "", args)
}

// ToolNodeIn is ToolNode with a workdir for nicer paths and on-disk file reads.
// DB reads now accept context via Context variants; cancellation is supported.
func ToolNodeIn(ctx context.Context, database *db.DB, workdir string, args NodeArgs) (*NodeResult, error) {
	name := strings.TrimSpace(args.Name)
	fileHint := strings.TrimSpace(args.File)

	if name == "" && fileHint == "" {
		return nil, fmt.Errorf("name or file is required")
	}

	// FILE READ MODE: file alone (no name) → on-disk source like Read + dependents.
	if name == "" && fileHint != "" {
		text, err := handleFileView(ctx, database, workdir, fileHint, args)
		if err != nil {
			return nil, err
		}
		return &NodeResult{
			Content:  []ContentItem{{Type: "text", Text: text}},
			FileMode: true,
		}, nil
	}

	includeCode := false
	if args.IncludeCode != nil {
		includeCode = *args.IncludeCode
	}

	nodes, err := findSymbolMatches(ctx, database, workdir, name, fileHint, args.Line)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return &NodeResult{
			Content: []ContentItem{{Type: "text", Text: fmt.Sprintf("Symbol %q not found in the codebase", name)}},
		}, nil
	}

	// Single definition — common case.
	if len(nodes) == 1 {
		text := renderNodeSection(ctx, database, workdir, nodes[0], includeCode)
		return &NodeResult{
			Content: []ContentItem{{Type: "text", Text: text}},
		}, nil
	}

	// Multiple overloads: pack bodies
	var rendered []string
	var listed []db.Node
	used := 0
	for _, n := range nodes {
		if len(rendered) >= nodeHardCap {
			listed = append(listed, n)
			continue
		}
		section := renderNodeSection(ctx, database, workdir, n, includeCode)
		if len(rendered) == 0 || used+len(section) <= nodeBodyBudget {
			rendered = append(rendered, section)
			used += len(section)
		} else {
			listed = append(listed, n)
		}
	}

	var b strings.Builder
	b.WriteString(strings.Join(rendered, "\n\n"))
	if len(listed) > 0 {
		fmt.Fprintf(&b, "\n... (%d more definitions)", len(listed))
	}
	return &NodeResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

func findSymbolMatches(ctx context.Context, database *db.DB, workdir, name, fileHint string, lineHint int) ([]db.Node, error) {
	nodes, err := database.GetNodeByNameContext(ctx, name)
	if err != nil {
		return nil, err
	}
	var defs []db.Node
	for _, n := range nodes {
		if n.Kind == db.KindFile || n.Kind == "module" || n.Kind == "import" || n.Kind == "export" {
			continue
		}
		defs = append(defs, n)
	}
	if len(defs) == 0 {
		return nil, nil
	}

	// Disambiguate by file/line — only narrows, never empties on bad hints.
	if len(defs) > 1 && (fileHint != "" || lineHint > 0) {
		narrowed := defs
		if fileHint != "" {
			var byFile []db.Node
			for _, n := range narrowed {
				if fileHintMatches(n.File, fileHint, workdir) {
					byFile = append(byFile, n)
				}
			}
			if len(byFile) > 0 {
				narrowed = byFile
			}
		}
		if lineHint > 0 && len(narrowed) > 1 {
			var containing []db.Node
			for _, n := range narrowed {
				end := n.EndLine
				if end < n.Line {
					end = n.Line
				}
				if n.Line <= lineHint && end >= lineHint {
					containing = append(containing, n)
				}
			}
			if len(containing) > 0 {
				narrowed = containing
			} else {
				// nearest start line
				best := narrowed[0]
				bestDist := absInt(best.Line - lineHint)
				for _, n := range narrowed[1:] {
					d := absInt(n.Line - lineHint)
					if d < bestDist {
						best = n
						bestDist = d
					}
				}
				narrowed = []db.Node{best}
			}
		}
		if len(narrowed) > 0 {
			defs = narrowed
		}
	}
	return defs, nil
}

func renderNodeSection(ctx context.Context, database *db.DB, workdir string, node db.Node, includeCode bool) string {
	var b strings.Builder
	// 精简输出：只显示位置和代码，像 Read 一样
	fmt.Fprintf(&b, "%s:%d\n", db.RelPath(workdir, node.File), node.Line)

	if includeCode && node.Body != "" {
		numbered := numberSourceLines(node.Body, node.Line)
		b.WriteString(numbered)
	}

	// 精简 trail：只显示名称和位置，不显示额外格式
	callers, callerErr := database.GetCallersWithKindContext(ctx, node.ID)
	callees, calleeErr := database.GetCalleesWithKindContext(ctx, node.ID)
	if callerErr != nil || calleeErr != nil {
		fmt.Fprintf(&b, "(error fetching callers/callees for %s)\n", node.Name)
	}
	if len(callers) > 0 || len(callees) > 0 {
		b.WriteString("\n")
		if len(callees) > 0 {
			parts := make([]string, 0, min(len(callees), 6))
			for i, c := range callees {
				if i >= 6 {
					break
				}
				parts = append(parts, fmt.Sprintf("%s:%d", c.Name, c.Line))
			}
			fmt.Fprintf(&b, "Calls: %s\n", strings.Join(parts, ", "))
		}
		if len(callers) > 0 {
			parts := make([]string, 0, min(len(callers), 6))
			for i, c := range callers {
				if i >= 6 {
					break
				}
				parts = append(parts, fmt.Sprintf("%s:%d", c.Name, c.Line))
			}
			fmt.Fprintf(&b, "Callers: %s\n", strings.Join(parts, ", "))
		}
	}
	return b.String()
}

func handleFileView(ctx context.Context, database *db.DB, workdir, fileArg string, args NodeArgs) (string, error) {
	resolved, candidates, err := resolveIndexedFile(ctx, database, workdir, fileArg)
	if err != nil {
		return "", err
	}
	if resolved == "" {
		if len(candidates) > 1 {
			var b strings.Builder
			fmt.Fprintf(&b, "%q matches %d indexed files — pass a longer path:\n\n", fileArg, len(candidates))
			for i, f := range candidates {
				if i >= 25 {
					break
				}
				fmt.Fprintf(&b, "- %s\n", db.RelPath(workdir, f))
			}
			return b.String(), nil
		}
		return fmt.Sprintf("No indexed file matches %q. Codegraph indexes source files; configs/docs it doesn't parse won't appear — Read those directly.", fileArg), nil
	}

	rel := db.RelPath(workdir, resolved)
	nodes, err := database.GetNodesByFileContext(ctx, resolved)
	if err != nil {
		return "", err
	}
	var symbols []db.Node
	for _, n := range nodes {
		if n.Kind == db.KindFile || n.Kind == "module" || n.Kind == "import" || n.Kind == "export" {
			continue
		}
		symbols = append(symbols, n)
	}
	sort.Slice(symbols, func(i, j int) bool { return symbols[i].Line < symbols[j].Line })

	dependents, depErr := database.GetFileDependentsContext(ctx, resolved)
	if depErr != nil {
		dependents = nil
	}
	depSummary := "no other indexed file depends on it"
	if len(dependents) > 0 {
		shown := dependents
		if len(shown) > 8 {
			shown = shown[:8]
		}
		rels := make([]string, len(shown))
		for i, d := range shown {
			rels[i] = db.RelPath(workdir, d)
		}
		depSummary = fmt.Sprintf("used by %d file%s: %s", len(dependents), plural(len(dependents)), strings.Join(rels, ", "))
		if len(dependents) > 8 {
			depSummary += fmt.Sprintf(", +%d more", len(dependents)-8)
		}
	}

	if args.SymbolsOnly {
		var b strings.Builder
		if len(dependents) > 0 {
			shown := dependents
			if len(shown) > 8 {
				shown = shown[:8]
			}
			rels := make([]string, len(shown))
			for i, d := range shown {
				rels[i] = db.RelPath(workdir, d)
			}
			fmt.Fprintf(&b, "used by %d: %s\n", len(dependents), strings.Join(rels, ", "))
		}
		if len(symbols) > 0 {
			for _, n := range symbols {
				fmt.Fprintf(&b, "%s %s:%d\n", n.Name, db.RelPath(workdir, n.File), n.Line)
			}
		} else {
			b.WriteString("(no indexed symbols)\n")
		}
		return b.String(), nil
	}

	// Read current bytes from disk (same shape as Read). Never leave workdir
	// (including via symlink escape — resolve to real path before read).
	abs, ok := safeReadPath(workdir, resolved)
	if !ok {
		return "(path outside workspace)\n", nil
	}
	// Check file size before reading to avoid memory spike on huge files.
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", rel, err)
	}
	if fi.Size() > 1<<20 { // 1 MB
		return "", fmt.Errorf("%s: file too large (%d bytes) — use Read or a narrower range", rel, fi.Size())
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}

	fileLines := strings.Split(string(content), "\n")
	total := len(fileLines)
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	if offset > total {
		return fmt.Sprintf("**%s** has %d line%s — offset %d is past the end. %s", rel, total, plural(total), offset, depSummary), nil
	}
	maxLines := args.Limit
	if maxLines <= 0 || maxLines > nodeFileLineDef {
		maxLines = nodeFileLineDef
	}

	var numbered []string
	start := offset - 1
	i := start
	for ; i < total && len(numbered) < maxLines; i++ {
		ln := fmt.Sprintf("%d\t%s", i+1, fileLines[i])
		numbered = append(numbered, ln)
	}
	shownEnd := start + len(numbered)
	complete := offset == 1 && shownEnd >= total

	var b strings.Builder
	b.WriteString(strings.Join(numbered, "\n"))
	if !complete {
		fmt.Fprintf(&b, "\n... (lines %d-%d of %d)", offset, shownEnd, total)
	}
	return b.String(), nil
}

// resolveIndexedFile finds one indexed path for a path/basename hint.
// When workdir is set, only files under workdir are considered (no ../ escape).
func resolveIndexedFile(ctx context.Context, database *db.DB, workdir, fileArg string) (resolved string, candidates []string, err error) {
	all, err := database.ListFilesContext(ctx)
	if err != nil {
		return "", nil, err
	}
	if len(all) == 0 {
		return "", nil, nil
	}

	// Scope to workdir when present — never surface/read outside the project root.
	if workdir != "" {
		var scoped []string
		for _, f := range all {
			if _, ok := pathWithinRoot(workdir, f); ok {
				scoped = append(scoped, f)
			}
		}
		all = scoped
		if len(all) == 0 {
			return "", nil, nil
		}
	}

	var hits []string
	for _, f := range all {
		if fileHintMatches(f, fileArg, workdir) {
			hits = append(hits, f)
		}
	}
	switch len(hits) {
	case 0:
		return "", nil, nil
	case 1:
		return hits[0], nil, nil
	default:
		return "", hits, nil
	}
}

// fileHintMatches reports whether indexed filePath is the file the agent meant
// by hint. Uses exact / path-boundary suffix / basename equality — never bare
// substring (avoids main.go→remain.go) or unanchored HasSuffix (avoids a.go→ba.go).
func fileHintMatches(filePath, hint, workdir string) bool {
	if strings.TrimSpace(hint) == "" || filePath == "" {
		return false
	}
	nf := normPath(filePath)
	hint = strings.TrimSpace(hint)

	// Candidate forms of the hint.
	var forms []string
	forms = append(forms, normPath(hint))
	if workdir != "" {
		if abs, ok := resolveHintUnderRoot(workdir, hint); ok {
			forms = append(forms, normPath(abs))
		}
	}
	// Relative multi-segment as cleaned slash path without forcing abs.
	hSlash := normPath(filepath.ToSlash(hint))
	hSlash = strings.TrimPrefix(hSlash, "./")
	if hSlash != "" {
		forms = append(forms, hSlash)
	}

	seen := map[string]bool{}
	for _, h := range forms {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if nf == h {
			return true
		}
		// Boundary-safe suffix: ".../src/a.go" matches hint "src/a.go"
		if strings.HasSuffix(nf, "/"+h) {
			return true
		}
	}

	// Basename-only hint (no slash): compare bases only.
	if !strings.Contains(hint, "/") && !strings.Contains(hint, "\\") && hint != "." && hint != ".." {
		return filepath.Base(nf) == strings.ToLower(filepath.Base(hint))
	}
	return false
}

// resolveHintUnderRoot joins/cleans hint under root and returns it only if still inside root.
func resolveHintUnderRoot(root, hint string) (string, bool) {
	if root == "" || hint == "" {
		return "", false
	}
	var target string
	if filepath.IsAbs(hint) {
		target = filepath.Clean(hint)
	} else {
		target = filepath.Clean(filepath.Join(root, hint))
	}
	return pathWithinRoot(root, target)
}

func normPath(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	return strings.TrimRight(strings.ToLower(p), "/")
}

func numberSourceLines(body string, startLine int) string {
	if startLine <= 0 {
		startLine = 1
	}
	lines := strings.Split(body, "\n")
	// Drop trailing empty from final newline so numbering matches body span.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = fmt.Sprintf("%d\t%s", startLine+i, ln)
	}
	return strings.Join(out, "\n")
}

func pathWithinRoot(root, target string) (string, bool) {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if root == "" || target == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", false
	}
	// Escape if rel is ".." or starts with "../"
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return target, true
}

// safeReadPath returns a path safe to os.ReadFile: under workdir after Clean,
// and after EvalSymlinks so a link cannot point outside the root.
// Empty workdir skips the jail (internal/tests only).
func safeReadPath(workdir, target string) (string, bool) {
	target = filepath.Clean(target)
	if workdir == "" {
		return target, true
	}
	root := filepath.Clean(workdir)
	if _, ok := pathWithinRoot(root, target); !ok {
		return "", false
	}
	// Resolve symlinks on existing paths; if the file is missing, still allow
	// the cleaned in-root path (caller handles read error).
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Dangling link or not yet created — do not follow; keep logical path
		// only if it stays in root (already checked).
		return target, true
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	if _, ok := pathWithinRoot(realRoot, realTarget); !ok {
		return "", false
	}
	return realTarget, true
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

package extraction

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ExtractedNode represents a symbol found in source code.
type ExtractedNode struct {
	Kind          string // function, class, method, struct, interface, type, variable, constant
	Name          string
	File          string
	Line          int
	EndLine       int
	Body          string
	Language      string
	QualifiedName string // e.g. pkg.Func, Class.method, Receiver.Method
	Signature     string // params (+ return) without the name
	Docstring     string
	Visibility    string // public / private / protected (when known)
	IsExported    bool
	ReturnType    string
	StartColumn   int
	EndColumn     int
}

// ExtractedEdge represents a relationship found in source code.
// Prefer imports/extends/implements here. Call sites should go into
// UnresolvedReference so the resolution pass owns cross-file linking.

// ExtractedEdge represents a relationship found in source code.
// Prefer imports/extends/implements here. Call sites should go into
// UnresolvedReference so the resolution pass owns cross-file linking.
type ExtractedEdge struct {
	SourceName string
	TargetName string
	Kind       string // calls, imports, extends, implements
	File       string
	Line       int
	Col        int
}

// UnresolvedReference is a named reference awaiting resolution.
// from-symbol is identified by name (+ optional def line) until nodes are inserted.

// UnresolvedReference is a named reference awaiting resolution.
// from-symbol is identified by name (+ optional def line) until nodes are inserted.
type UnresolvedReference struct {
	FromName      string // enclosing symbol name (empty = file-level)
	FromLine      int    // enclosing symbol def line (0 if unknown)
	ReferenceName string
	ReferenceKind string // calls, references, imports, ...
	Line          int
	Col           int
	FilePath      string
	Language      string
	Candidates    []string
}

// ExtractResult is the full extractor output (step 2 model).

// ExtractResult is the full extractor output (step 2 model).
type ExtractResult struct {
	Nodes []ExtractedNode
	Edges []ExtractedEdge       // imports etc.; same-file calls may remain briefly
	Refs  []UnresolvedReference // call/type refs for resolution
}

// Extractor extracts symbols and edges from source code.

// Extractor extracts symbols and edges from source code.
type Extractor struct {
	language string
}

// NewExtractor creates an extractor for the given language.

// NewExtractor creates an extractor for the given language.
func NewExtractor(language string) *Extractor {
	return &Extractor{language: language}
}

// Extract parses the source code and returns nodes, structural edges, and pending refs.
// The regex extractor is best-effort and never fails; the error return keeps
// the signature uniform with TreeSitterExtractor so callers can fall back.

// Extract parses the source code and returns nodes, structural edges, and pending refs.
// The regex extractor is best-effort and never fails; the error return keeps
// the signature uniform with TreeSitterExtractor so callers can fall back.
func (e *Extractor) Extract(source string, filePath string) (ExtractResult, error) {
	source = normalizeSource(source)
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	switch e.language {
	case "go":
		nodes, edges = e.extractGo(source, filePath)
	case "typescript", "javascript":
		nodes, edges = e.extractJS(source, filePath)
	case "python":
		nodes, edges = e.extractPython(source, filePath)
	case "objective-c":
		nodes, edges = e.extractObjC(source, filePath)
	case "svelte", "vue", "astro":
		// Full SFC result (component node + scripts + template refs).
		return e.extractSFC(source, filePath)
	case "liquid":
		nodes, edges = e.extractLiquid(source, filePath)
	case "luau":
		nodes, edges = e.extractLua(source, filePath)
	case "pascal":
		nodes, edges = e.extractPascal(source, filePath)
	case "rust":
		nodes, edges = e.extractRust(source, filePath)
	default:
		nodes, edges = e.extractGeneric(source, filePath)
	}
	return promoteCallsToRefs(nodes, edges, filePath, e.language), nil
}

// normalizeSource strips a UTF-8 BOM and normalizes CRLF line endings to LF
// so regex anchors (^func …), string comparisons and line-based scanners see
// the same bytes regardless of file encoding (audit: BOM/CRLF handling).

// normalizeSource strips a UTF-8 BOM and normalizes CRLF line endings to LF
// so regex anchors (^func …), string comparisons and line-based scanners see
// the same bytes regardless of file encoding (audit: BOM/CRLF handling).
func normalizeSource(source string) string {
	source = strings.TrimPrefix(source, "\uFEFF")
	if strings.Contains(source, "\r\n") {
		source = strings.ReplaceAll(source, "\r\n", "\n")
	}
	return source
}

// promoteCallsToRefs moves call edges into UnresolvedReference so the
// orchestrator can same-file-link or park them as pending (step 2).

// promoteCallsToRefs moves call edges into UnresolvedReference so the
// orchestrator can same-file-link or park them as pending (step 2).
func promoteCallsToRefs(nodes []ExtractedNode, edges []ExtractedEdge, filePath, lang string) ExtractResult {
	out := ExtractResult{Nodes: nodes}
	// Map symbol name → its definitions sorted by line, so the enclosing
	// symbol of each call site can be picked by source range instead of a
	// first-wins name lookup (two same-named methods must not steal each
	// other's calls).
	byName := make(map[string][]ExtractedNode, len(nodes))
	for _, n := range nodes {
		byName[n.Name] = append(byName[n.Name], n)
	}
	for name := range byName {
		sort.Slice(byName[name], func(i, j int) bool {
			return byName[name][i].Line < byName[name][j].Line
		})
	}
	for _, e := range edges {
		if e.Kind == "calls" {
			// Do NOT drop noisy names here: same-file link needs the ref first
			// (e.g. add/new/close). Noise is filtered only when parking cross-file
			// unknowns in parkUnresolved.
			fromLine := 0
			if ns := byName[e.SourceName]; len(ns) > 0 {
				fromLine = enclosingDefLine(ns, e.Line)
			}
			out.Refs = append(out.Refs, UnresolvedReference{
				FromName:      e.SourceName,
				FromLine:      fromLine,
				ReferenceName: e.TargetName,
				ReferenceKind: "calls",
				Line:          e.Line,
				Col:           e.Col,
				FilePath:      filePath,
				Language:      lang,
			})
			continue
		}
		out.Edges = append(out.Edges, e)
	}
	return out
}

// enclosingDefLine picks the definition line of the symbol whose source range
// contains callLine (innermost wins). Falls back to the nearest definition at
// or before the call line, then to the first definition, so a wrong end-line
// never degrades attribution to a file-level ref. Returns 0 when no definition
// exists for the name (caller treats the ref as file-level).

// enclosingDefLine picks the definition line of the symbol whose source range
// contains callLine (innermost wins). Falls back to the nearest definition at
// or before the call line, then to the first definition, so a wrong end-line
// never degrades attribution to a file-level ref. Returns 0 when no definition
// exists for the name (caller treats the ref as file-level).
func enclosingDefLine(nodes []ExtractedNode, callLine int) int {
	var contain *ExtractedNode
	for i := range nodes {
		n := &nodes[i]
		end := n.EndLine
		if end == 0 {
			end = n.Line
		}
		if n.Line <= callLine && callLine <= end {
			if contain == nil || n.Line >= contain.Line {
				contain = n
			}
		}
	}
	if contain != nil {
		return contain.Line
	}
	var best *ExtractedNode
	for i := range nodes {
		n := &nodes[i]
		if n.Line <= callLine && (best == nil || n.Line > best.Line) {
			best = n
		}
	}
	if best != nil {
		return best.Line
	}
	if len(nodes) > 0 {
		return nodes[0].Line
	}
	return 0
}

// NameTail returns the last segment of a dotted/qualified reference name.

// NameTail returns the last segment of a dotted/qualified reference name.
func NameTail(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndexAny(name, "./#@"); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

// appendCallEdges scans body lines [startIdx, endIdx) for call sites and
// appends one calls edge per distinct name:line hit, stamped with the
// absolute call-site line (not the enclosing function's definition line).
// exclude holds names that must not become targets (the function's own name,
// matching the tree-sitter path); isKeyword filters language keywords.

// appendCallEdges scans body lines [startIdx, endIdx) for call sites and
// appends one calls edge per distinct name:line hit, stamped with the
// absolute call-site line (not the enclosing function's definition line).
// exclude holds names that must not become targets (the function's own name,
// matching the tree-sitter path); isKeyword filters language keywords.
func appendCallEdges(edges *[]ExtractedEdge, filePath, sourceName string, lines []string, startIdx, endIdx int, callRe *regexp.Regexp, exclude map[string]bool, isKeyword func(string) bool) {
	seen := map[string]bool{}
	for li := startIdx; li < endIdx && li < len(lines); li++ {
		for _, m := range callRe.FindAllStringSubmatch(lines[li], -1) {
			if len(m) < 2 || exclude[m[1]] || isKeyword(m[1]) {
				continue
			}
			key := fmt.Sprintf("%s:%d", m[1], li+1)
			if seen[key] {
				continue
			}
			seen[key] = true
			*edges = append(*edges, ExtractedEdge{
				SourceName: sourceName,
				TargetName: m[1],
				Kind:       "calls",
				File:       filePath,
				Line:       li + 1,
			})
		}
	}
}

// ---------- Go extraction ----------

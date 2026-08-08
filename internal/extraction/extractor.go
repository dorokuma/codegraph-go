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
type ExtractResult struct {
	Nodes []ExtractedNode
	Edges []ExtractedEdge       // imports etc.; same-file calls may remain briefly
	Refs  []UnresolvedReference // call/type refs for resolution
}

// Extractor extracts symbols and edges from source code.
type Extractor struct {
	language string
}

// NewExtractor creates an extractor for the given language.
func NewExtractor(language string) *Extractor {
	return &Extractor{language: language}
}

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
func normalizeSource(source string) string {
	source = strings.TrimPrefix(source, "\uFEFF")
	if strings.Contains(source, "\r\n") {
		source = strings.ReplaceAll(source, "\r\n", "\n")
	}
	return source
}

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

var (
	goFuncRe   = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?(\w+)\s*\(`)
	goTypeRe   = regexp.MustCompile(`^type\s+(\w+)\s+(struct|interface)\b`)
	goCallRe   = regexp.MustCompile(`\b(\w+)\s*\(`)
	goImportRe = regexp.MustCompile(`"([^"]+)"`)
)

func (e *Extractor) extractGo(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	inImport := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Track import block
		if strings.HasPrefix(trimmed, "import (") {
			inImport = true
			continue
		}
		if inImport {
			if trimmed == ")" {
				inImport = false
				continue
			}
			if matches := goImportRe.FindStringSubmatch(trimmed); len(matches) > 1 {
				edges = append(edges, ExtractedEdge{
					SourceName: filePath,
					TargetName: matches[1],
					Kind:       "imports",
					File:       filePath,
					Line:       lineNum,
				})
			}
			continue
		}

		// Single import
		if strings.HasPrefix(trimmed, "import ") {
			if matches := goImportRe.FindStringSubmatch(trimmed); len(matches) > 1 {
				edges = append(edges, ExtractedEdge{
					SourceName: filePath,
					TargetName: matches[1],
					Kind:       "imports",
					File:       filePath,
					Line:       lineNum,
				})
			}
			continue
		}

		// Type declarations
		if matches := goTypeRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			kind := "struct"
			if matches[2] == "interface" {
				kind = "interface"
			}
			endLine := findBraceEnd(lines, i)
			nodes = append(nodes, ExtractedNode{
				Kind:     kind,
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     extractBody(lines, i, endLine),
				Language: "go",
			})
			continue
		}

		// Function declarations
		if matches := goFuncRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findBraceEnd(lines, i)
			kind := "function"
			// Check if it's a method (has receiver)
			if strings.Contains(trimmed, "(") && strings.Index(trimmed, "(") < strings.Index(trimmed, matches[1]) {
				// Has receiver, it's a method
				kind = "method"
			}
			nodes = append(nodes, ExtractedNode{
				Kind:     kind,
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     extractBody(lines, i, endLine),
				Language: "go",
			})

			// Extract function calls from body with call-site line numbers.
			appendCallEdges(&edges, filePath, matches[1], lines, i, endLine, goCallRe, map[string]bool{matches[1]: true}, isGoKeyword)
			continue
		}
	}

	return nodes, edges
}

// ---------- TypeScript/JavaScript extraction ----------

var (
	jsFuncRe   = regexp.MustCompile(`(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*\(`)
	jsClassRe  = regexp.MustCompile(`(?:export\s+)?class\s+(\w+)`)
	jsCallRe   = regexp.MustCompile(`\b(\w+)\s*\(`)
	jsImportRe = regexp.MustCompile(`(?:from\s+['"]([^'"]+)['"]|require\s*\(\s*['"]([^'"]+)['"]\s*\))`)
	// Arrow functions assigned to a name: const f = (...) => … (fallback path).
	jsArrowRe = regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`)
	// Class-body methods: optional TS/JS modifiers, optional get/set prefix.
	jsMethodRe = regexp.MustCompile(`^(?:(?:public|private|protected|static|async)\s+)*(?:get\s+|set\s+)?([A-Za-z_$][\w$]*)\s*\(`)
)

func (e *Extractor) extractJS(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Imports
		if matches := jsImportRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			target := matches[1]
			if target == "" {
				target = matches[2]
			}
			if target != "" {
				edges = append(edges, ExtractedEdge{
					SourceName: filePath,
					TargetName: target,
					Kind:       "imports",
					File:       filePath,
					Line:       lineNum,
				})
			}
			continue
		}

		// Class declarations
		if matches := jsClassRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findBraceEnd(lines, i)
			nodes = append(nodes, ExtractedNode{
				Kind:     "class",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     extractBody(lines, i, endLine),
				Language: e.language,
			})
			// Methods inside the class body (fallback parity with tree-sitter).
			e.extractJSClassMethods(&nodes, &edges, lines, i+1, endLine, filePath, matches[1])
			continue
		}

		// Arrow functions assigned to const/let/var.
		if matches := jsArrowRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findBraceEnd(lines, i)
			kind := "function"
			if len(matches[1]) > 0 && matches[1][0] >= 'A' && matches[1][0] <= 'Z' {
				kind = "component"
			}
			nodes = append(nodes, ExtractedNode{
				Kind:     kind,
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     extractBody(lines, i, endLine),
				Language: e.language,
			})
			appendCallEdges(&edges, filePath, matches[1], lines, i, endLine, jsCallRe, map[string]bool{matches[1]: true}, isJSKeyword)
			continue
		}

		// Function declarations
		if matches := jsFuncRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findBraceEnd(lines, i)
			body := extractBody(lines, i, endLine)
			nodes = append(nodes, ExtractedNode{
				Kind:     "function",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     body,
				Language: e.language,
			})

			// Extract calls with call-site line numbers.
			appendCallEdges(&edges, filePath, matches[1], lines, i, endLine, jsCallRe, map[string]bool{matches[1]: true}, isJSKeyword)
			continue
		}
	}

	return nodes, edges
}

// extractJSClassMethods extracts method definitions inside a class body
// (regex fallback; the tree-sitter path handles methods natively). Only
// class-body members at brace depth 1 are treated as methods: method bodies
// can contain bare calls like `helper();` that satisfy jsMethodRe, and
// without a depth check they would be misread as sibling methods with fake
// contains edges (must-fix: the previous version scanned the whole class
// range without tracking brace depth).
func (e *Extractor) extractJSClassMethods(nodes *[]ExtractedNode, edges *[]ExtractedEdge, lines []string, startIdx, endIdx int, filePath, className string) {
	sc := braceScanner{depth: 1} // class body members live at depth 1
	for li := startIdx; li < endIdx && li < len(lines); li++ {
		memberDepth := sc.depth
		sc.scan(lines[li])
		if memberDepth != 1 {
			continue // inside a method/block body — not a member line
		}
		trimmed := strings.TrimSpace(lines[li])
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		m := jsMethodRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		name := m[1]
		if isJSKeyword(name) {
			continue // e.g. `if (`, `for (` — statement, not a method
		}
		methodEnd := findBraceEnd(lines, li)
		*nodes = append(*nodes, ExtractedNode{
			Kind:          "method",
			Name:          name,
			File:          filePath,
			Line:          li + 1,
			EndLine:       methodEnd,
			Body:          extractBody(lines, li, methodEnd),
			Language:      e.language,
			QualifiedName: className + "." + name,
		})
		*edges = append(*edges, ExtractedEdge{
			SourceName: className,
			TargetName: name,
			Kind:       "contains",
			File:       filePath,
			Line:       li + 1,
		})
		appendCallEdges(edges, filePath, name, lines, li, methodEnd, jsCallRe, map[string]bool{name: true}, isJSKeyword)
	}
}

// ---------- Python extraction ----------

var (
	pyFuncRe   = regexp.MustCompile(`^def\s+(\w+)\s*\(`)
	pyClassRe  = regexp.MustCompile(`^class\s+(\w+)`)
	pyCallRe   = regexp.MustCompile(`\b(\w+)\s*\(`)
	pyImportRe = regexp.MustCompile(`(?:from\s+(\S+)\s+import|import\s+(\S+))`)
)

func (e *Extractor) extractPython(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Imports
		if matches := pyImportRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			target := matches[1]
			if target == "" {
				target = matches[2]
			}
			if target != "" {
				edges = append(edges, ExtractedEdge{
					SourceName: filePath,
					TargetName: target,
					Kind:       "imports",
					File:       filePath,
					Line:       lineNum,
				})
			}
			continue
		}

		// Class declarations
		if matches := pyClassRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findIndentEnd(lines, i)
			nodes = append(nodes, ExtractedNode{
				Kind:     "class",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     extractBody(lines, i, endLine),
				Language: "python",
			})
			continue
		}

		// Function declarations
		if matches := pyFuncRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findIndentEnd(lines, i)
			body := extractBody(lines, i, endLine)
			nodes = append(nodes, ExtractedNode{
				Kind:     "function",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     body,
				Language: "python",
			})

			// Extract calls with call-site line numbers.
			appendCallEdges(&edges, filePath, matches[1], lines, i, endLine, pyCallRe, map[string]bool{matches[1]: true}, isPythonKeyword)
			continue
		}
	}

	return nodes, edges
}

// ---------- Rust extraction (regex; enough for use + fn + calls + metadata) ----------

var (
	// Head matches up to (but not including) the parameter list; the params are
	// extracted with a balanced-paren scan (rustFnHeadRe + readParenGroup) so
	// nested parens in signatures (impl Fn(i32) -> i32, fn pointers) work.
	rustFnHeadRe = regexp.MustCompile(`^(?:pub(?:\s*\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+(\w+)\s*(?:<[^>]*>)?`)
	rustStructRe = regexp.MustCompile(`^(pub(?:\s*\([^)]*\))?\s+)?(struct|enum|trait)\s+(\w+)`)
	rustImplRe   = regexp.MustCompile(`^impl(?:\s*<[^>]*>)?\s+(?:(?:[\w:]+)\s+for\s+)?([\w]+)`)
	rustUseRe    = regexp.MustCompile(`^use\s+(.+?);\s*$`)
	rustCallRe   = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	rustPubRe    = regexp.MustCompile(`^pub(?:\s*\([^)]*\))?\s+`)
)

func rustIsPub(line string) bool {
	return rustPubRe.MatchString(strings.TrimSpace(line))
}

func rustVisibility(line string) string {
	if rustIsPub(line) {
		return "public"
	}
	return "private"
}

func (e *Extractor) extractRust(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	// Track impl block ranges: startLine (1-based) → type name, endLine inclusive.
	type implRange struct {
		start, end int
		recv       string
	}
	var impls []implRange
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := rustImplRe.FindStringSubmatch(trimmed); len(m) > 1 {
			end := findBraceEnd(lines, i)
			impls = append(impls, implRange{start: i + 1, end: end, recv: m[1]})
		}
	}
	implRecvAt := func(lineNum int) string {
		for _, ir := range impls {
			if lineNum >= ir.start && lineNum <= ir.end {
				return ir.recv
			}
		}
		return ""
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}

		// use paths → imports (crate / workspace member / module path)
		if matches := rustUseRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			for _, path := range splitRustUsePaths(matches[1]) {
				edges = append(edges, ExtractedEdge{
					SourceName: filePath,
					TargetName: path,
					Kind:       "imports",
					File:       filePath,
					Line:       lineNum,
				})
			}
			continue
		}

		// Skip bare impl headers (tracked above); don't treat as structs.
		if rustImplRe.MatchString(trimmed) {
			continue
		}

		if matches := rustStructRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			kind := matches[2]
			name := matches[3]
			if kind == "trait" {
				kind = "interface"
			}
			endLine := findBraceEnd(lines, i)
			exported := matches[1] != ""
			vis := "private"
			if exported {
				vis = "public"
			}
			nodes = append(nodes, ExtractedNode{
				Kind:          kind,
				Name:          name,
				File:          filePath,
				Line:          lineNum,
				EndLine:       endLine,
				Body:          extractBody(lines, i, endLine),
				Language:      "rust",
				QualifiedName: name,
				Visibility:    vis,
				IsExported:    exported,
			})
			continue
		}

		if matches := rustFnHeadRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			name := matches[1]
			// Balanced scan for the parameter list (supports nested parens like
			// impl Fn(i32) -> i32 and fn-pointer parameter types).
			params, after := readParenGroup(trimmed[len(matches[0]):])
			if params == "" {
				continue // no parameter list — not a function definition
			}
			// Return type: only when the remainder starts with -> (a `where`
			// clause or attribute must not be mistaken for a return type).
			ret := strings.TrimSpace(after)
			if strings.HasPrefix(ret, "->") {
				ret = strings.TrimSpace(strings.TrimPrefix(ret, "->"))
				if i := strings.IndexAny(ret, "{;"); i >= 0 {
					ret = ret[:i]
				}
				ret = strings.TrimSpace(ret)
			} else {
				ret = ""
			}
			endLine := findBraceEnd(lines, i)
			body := extractBody(lines, i, endLine)
			sig := params
			if ret != "" {
				sig = params + " -> " + ret
			}
			// Normalize return to bare type tail (Foo::Bar → Bar; strip refs).
			retType := ret
			retType = strings.TrimPrefix(retType, "&")
			retType = strings.TrimPrefix(retType, "mut ")
			if j := strings.IndexAny(retType, "[<"); j >= 0 {
				retType = retType[:j]
			}
			if j := strings.LastIndex(retType, "::"); j >= 0 {
				retType = retType[j+2:]
			}
			retType = strings.TrimSpace(retType)

			kind := "function"
			qn := name
			if recv := implRecvAt(lineNum); recv != "" {
				kind = "method"
				qn = recv + "." + name
				edges = append(edges, ExtractedEdge{
					SourceName: recv,
					TargetName: name,
					Kind:       "contains",
					File:       filePath,
					Line:       lineNum,
				})
			}
			exported := rustIsPub(trimmed)
			nodes = append(nodes, ExtractedNode{
				Kind:          kind,
				Name:          name,
				File:          filePath,
				Line:          lineNum,
				EndLine:       endLine,
				Body:          body,
				Language:      "rust",
				QualifiedName: qn,
				Signature:     sig,
				Visibility:    rustVisibility(trimmed),
				IsExported:    exported,
				ReturnType:    retType,
			})
			// calls inside body — call-site line numbers (fallback path parity
			// with the tree-sitter extractor).
			appendCallEdges(&edges, filePath, name, lines, i, endLine, rustCallRe, map[string]bool{name: true}, isRustKeyword)
		}
	}
	return nodes, edges
}

// readParenGroup scans s from its first '(' to the matching ')' (depth-aware,
// honoring string literals) and returns the full parenthesized group plus the
// remainder after it. Returns ("", "") when no group starts in s.
func readParenGroup(s string) (group, rest string) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return "", ""
	}
	depth := 0
	inString := byte(0)
	for i := open; i < len(s); i++ {
		ch := s[i]
		if inString != 0 {
			if ch == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			inString = ch
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open : i+1], s[i+1:]
			}
		}
	}
	return "", ""
}

// splitRustUsePaths expands `use a::{b, c::d}` / `use a::b` into import specs.
func splitRustUsePaths(spec string) []string {
	spec = strings.TrimSpace(spec)
	spec = strings.TrimPrefix(spec, "pub ")
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	// Nested brace groups: prefix::{a, b::c}
	if i := strings.Index(spec, "::{"); i >= 0 && strings.HasSuffix(spec, "}") {
		prefix := spec[:i]
		inner := strings.TrimSuffix(spec[i+3:], "}")
		var out []string
		for _, part := range splitTopLevelComma(inner) {
			part = strings.TrimSpace(part)
			if part == "" || part == "self" {
				out = append(out, prefix)
				continue
			}
			// rename: foo as bar → foo
			if j := strings.Index(part, " as "); j >= 0 {
				part = strings.TrimSpace(part[:j])
			}
			if part == "*" {
				out = append(out, prefix)
				continue
			}
			out = append(out, prefix+"::"+part)
		}
		return out
	}
	// Simple path; drop `as alias`
	if j := strings.Index(spec, " as "); j >= 0 {
		spec = strings.TrimSpace(spec[:j])
	}
	return []string{spec}
}

func splitTopLevelComma(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{', '(':
			depth++
		case '}', ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func isRustKeyword(s string) bool {
	switch s {
	case "if", "else", "for", "while", "loop", "match", "return", "break",
		"continue", "move", "async", "await", "unsafe", "as", "in", "ref",
		"mut", "let", "const", "static", "fn", "struct", "enum", "trait",
		"impl", "mod", "use", "pub", "crate", "super", "self", "Self",
		"true", "false", "where", "type", "Box", "Some", "None", "Ok", "Err",
		"Vec", "String", "println", "format", "panic", "assert", "assert_eq",
		"dbg", "todo", "unimplemented", "drop", "clone", "into", "from":
		return true
	}
	return false
}

// ---------- Generic extraction (fallback) ----------

var (
	genericFuncRe  = regexp.MustCompile(`(?:function|def|fn|func)\s+(\w+)\s*\(`)
	genericClassRe = regexp.MustCompile(`(?:class|struct|interface)\s+(\w+)`)
	// C/C++ definitions: void foo(, int bar(, static inline const char *baz(
	cStyleFuncRe = regexp.MustCompile(`^(?:(?:static|inline|extern|const|unsigned|signed|volatile)\s+)*(?:void|int|char|float|double|long|short|bool|size_t|ssize_t|uint\d+_t|int\d+_t|[\w:]+)\s*\*?\s+(\w+)\s*\(`)
)

func (e *Extractor) extractGeneric(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		if matches := genericFuncRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findBraceEnd(lines, i)
			nodes = append(nodes, ExtractedNode{
				Kind:     "function",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     extractBody(lines, i, endLine),
				Language: e.language,
			})
			continue
		}

		// C/C++ free functions (needed so cross-lang bridge can hang on a real node).
		if e.language == "c" || e.language == "cpp" {
			if matches := cStyleFuncRe.FindStringSubmatch(trimmed); len(matches) > 1 {
				name := matches[1]
				// Skip common non-function noise.
				if name != "if" && name != "for" && name != "while" && name != "switch" && !strings.HasSuffix(trimmed, ";") {
					endLine := findBraceEnd(lines, i)
					nodes = append(nodes, ExtractedNode{
						Kind:     "function",
						Name:     name,
						File:     filePath,
						Line:     lineNum,
						EndLine:  endLine,
						Body:     extractBody(lines, i, endLine),
						Language: e.language,
					})
					continue
				}
			}
		}

		if matches := genericClassRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			endLine := findBraceEnd(lines, i)
			nodes = append(nodes, ExtractedNode{
				Kind:     "class",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  endLine,
				Body:     extractBody(lines, i, endLine),
				Language: e.language,
			})
		}
	}

	return nodes, nil
}

// ---------- helpers ----------

// maxBraceScanLines caps findBraceEnd's forward scan: an unterminated '{'
// must not force a scan to EOF for every symbol of a pathological file
// (CPU amplification). Past the budget the declaration line alone is
// returned, like an unterminated block.
const maxBraceScanLines = 20000

// braceScanner is the shared line scanner behind findBraceEnd and
// extractJSClassMethods: it tracks brace depth while ignoring braces inside
// strings, comments and JS regex literals.
type braceScanner struct {
	depth          int
	inString       bool
	stringChar     byte
	triple         bool // Python """ / ''' string
	inLineComment  bool
	inBlockComment bool
}

// scan advances the scanner over one line and reports whether the brace
// depth dropped to zero inside it (i.e. the tracked block closed).
func (s *braceScanner) scan(line string) bool {
	s.inLineComment = false // comment state is per-line
	for j := 0; j < len(line); j++ {
		ch := line[j]
		if s.inLineComment {
			break // rest of the line is comment
		}
		if s.inBlockComment {
			if ch == '*' && j+1 < len(line) && line[j+1] == '/' {
				s.inBlockComment = false
				j++
			}
			continue
		}
		if s.inString {
			if s.triple {
				if ch == s.stringChar && j+2 < len(line) && line[j+1] == s.stringChar && line[j+2] == s.stringChar {
					s.inString = false
					s.triple = false
					j += 2
				}
				continue
			}
			// Go raw strings (backticks) do not treat '\' as an escape:
			// skipping the next char could swallow the closing backtick and
			// leave the string open until EOF (must-fix).
			if ch == '\\' && j+1 < len(line) && s.stringChar != '`' {
				j++ // skip escaped char
				continue
			}
			if ch == s.stringChar {
				s.inString = false
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			// Python triple-quoted strings: braces inside """...""" must
			// not count as block braces (must-fix).
			if j+2 < len(line) && line[j+1] == ch && line[j+2] == ch {
				s.inString = true
				s.stringChar = ch
				s.triple = true
				j += 2
				continue
			}
			s.inString = true
			s.stringChar = ch
			s.triple = false
			continue
		}
		if ch == '`' {
			s.inString = true
			s.stringChar = ch
			s.triple = false
			continue
		}
		if ch == '/' && j+1 < len(line) {
			if line[j+1] == '/' {
				s.inLineComment = true
				j++
				continue
			}
			if line[j+1] == '*' {
				s.inBlockComment = true
				j++
				continue
			}
			// JS regex literal: braces inside /.../ are not block braces
			// (must-fix). Division '/' is preceded by an operand and is not
			// treated as a regex start.
			if isRegexStart(line, j) {
				j = scanRegexLiteral(line, j)
				continue
			}
		}
		if ch == '{' {
			s.depth++
		} else if ch == '}' && s.depth > 0 {
			s.depth--
			if s.depth == 0 {
				return true
			}
		}
	}
	return false
}

// isRegexStart reports whether the '/' at line[j] starts a JS regex literal
// rather than a division. Heuristic: a '/' directly after an operand
// (identifier, number, closing bracket/quote, or a ++/-- prefix) is division;
// after operators, openers or at line start it is a regex literal.
func isRegexStart(line string, j int) bool {
	k := j - 1
	for k >= 0 && (line[k] == ' ' || line[k] == '\t') {
		k--
	}
	if k < 0 {
		return true // start of line: regex
	}
	prev := line[k]
	switch {
	case prev >= 'a' && prev <= 'z', prev >= 'A' && prev <= 'Z', prev >= '0' && prev <= '9',
		prev == '_', prev == '$', prev == ')', prev == ']', prev == '}',
		prev == '\'', prev == '"', prev == '`':
		return false // after an operand: division
	case prev == '+' || prev == '-':
		// a++ / b is division; + /re/ after an operator is a regex.
		k2 := k - 1
		for k2 >= 0 && (line[k2] == ' ' || line[k2] == '\t') {
			k2--
		}
		if k2 >= 0 && (line[k2] == '+' || line[k2] == '-') {
			return false // ++ / -- prefix: operand, so division
		}
		return true
	}
	return true
}

// scanRegexLiteral scans a JS regex literal starting at the '/' in line[j]
// and returns the index of its closing '/'. Escapes and character classes
// are honored; an unterminated regex consumes the rest of the line.
func scanRegexLiteral(line string, j int) int {
	inClass := false
	for j++; j < len(line); j++ {
		c := line[j]
		if c == '\\' {
			j++
			continue
		}
		if c == '[' {
			inClass = true
			continue
		}
		if c == ']' {
			inClass = false
			continue
		}
		if c == '/' && !inClass {
			return j
		}
	}
	return j // unterminated: consume to end of line
}

// findBraceEnd finds the line where the matching closing brace is (returned
// as the 1-based line of the closing brace, i.e. an exclusive end index).
// Braces inside strings, char literals, line comments (//), block comments
// (/* */) and JS regex literals are ignored, so a comment/string/regex
// containing '{' or '}' can no longer truncate or stretch a symbol's range.
// The forward scan is capped at maxBraceScanLines: an unterminated block
// returns start+1 (the declaration line only) instead of scanning a
// pathological file to EOF.
func findBraceEnd(lines []string, start int) int {
	sc := braceScanner{}
	limit := start + maxBraceScanLines
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := start; i < limit; i++ {
		if sc.scan(lines[i]) {
			return i + 1
		}
	}
	return start + 1
}

// findIndentEnd finds the end of an indented block (Python). Blank lines and
// comment-only lines after the header are skipped when measuring the block's
// base indent, so `def f():` followed by an empty line (or a docstring/comment
// line) no longer collapses the body to a single line.
func findIndentEnd(lines []string, start int) int {
	if start+1 >= len(lines) {
		return start + 1
	}

	baseIndent := -1
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		baseIndent = countIndent(lines[i])
		if baseIndent == 0 {
			return start + 1 // next statement is dedented — empty body
		}
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if countIndent(lines[j]) < baseIndent {
				return j
			}
		}
		return len(lines)
	}
	return start + 1
}

func countIndent(line string) int {
	count := 0
	for _, ch := range line {
		if ch == ' ' || ch == '\t' {
			count++
		} else {
			break
		}
	}
	return count
}

func extractBody(lines []string, start, end int) string {
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

// isGoKeyword is ONLY real language keywords (not builtins). Builtins like
// close/new/make may be user-defined; those calls must stay extractable.
func isGoKeyword(s string) bool {
	switch s {
	case "if", "else", "for", "range", "switch", "case", "default", "select",
		"defer", "go", "goto", "return", "break", "continue", "fallthrough",
		"var", "const", "type", "func", "struct", "interface", "map", "chan",
		"package", "import",
		"true", "false", "nil",
		"_":
		return true
	}
	return false
}

// isJSKeyword is ONLY syntactic keywords / literals. Host builtins (console,
// Promise, Array…) are not filtered here — unresolved noise is scrubbed after
// resolve when no project symbol matches.
func isJSKeyword(s string) bool {
	switch s {
	case "if", "else", "for", "while", "do", "switch", "case", "default",
		"break", "continue", "return", "throw", "try", "catch", "finally",
		"var", "let", "const", "function", "class",
		"new", "delete", "typeof", "instanceof", "void",
		"this", "super", "import", "export", "from",
		"async", "await", "yield",
		"null", "undefined", "true", "false",
		"debugger", "with", "in", "of":
		return true
	}
	return false
}

// isPythonKeyword is ONLY real keywords / literals (not builtins like print/len).
func isPythonKeyword(s string) bool {
	switch s {
	case "if", "elif", "else", "for", "while", "break", "continue", "pass",
		"return", "yield", "raise", "try", "except", "finally", "with", "as",
		"def", "class", "lambda", "and", "or", "not", "is", "in",
		"True", "False", "None",
		"import", "from", "global", "nonlocal", "assert", "del",
		"async", "await", "match", "case":
		return true
	}
	return false
}

// ---------- Objective-C extraction ----------

var (
	objcMethodRe = regexp.MustCompile(`^[-+]\s*\([^)]+\)\s*(\w+)`)
	objcClassRe  = regexp.MustCompile(`@interface\s+(\w+)`)
	objcImportRe = regexp.MustCompile(`#import\s+[<"]([^>"]+)[>"]`)
)

func (e *Extractor) extractObjC(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Import
		if matches := objcImportRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			edges = append(edges, ExtractedEdge{
				SourceName: filePath,
				TargetName: matches[1],
				Kind:       "imports",
				File:       filePath,
				Line:       lineNum,
			})
			continue
		}

		// Class declaration
		if matches := objcClassRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "class",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "objective-c",
			})
			continue
		}

		// Method declaration
		if matches := objcMethodRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "method",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "objective-c",
			})
		}
	}

	return nodes, edges
}

// ---------- Liquid extraction ----------

var (
	liquidSectionRe = regexp.MustCompile(`{%\s*section\s+['"]([^'"]+)['"]\s*%}`)
	liquidSnippetRe = regexp.MustCompile(`{%\s*snippet\s+['"]([^'"]+)['"]\s*%}`)
)

func (e *Extractor) extractLiquid(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode

	for i, line := range lines {
		lineNum := i + 1

		// Section reference
		if matches := liquidSectionRe.FindStringSubmatch(line); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "section",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "liquid",
			})
		}

		// Snippet reference
		if matches := liquidSnippetRe.FindStringSubmatch(line); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "snippet",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "liquid",
			})
		}
	}

	return nodes, nil
}

// ---------- Lua/Luau extraction ----------

var (
	luaFuncRe    = regexp.MustCompile(`(?:local\s+)?function\s+(\w+(?:\.\w+)*)\s*\(`)
	luaMethodRe  = regexp.MustCompile(`function\s+(\w+):(\w+)\s*\(`)
	luaRequireRe = regexp.MustCompile(`require\s*\(\s*['"]([^'"]+)['"]\s*\)`)
)

func (e *Extractor) extractLua(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Require
		if matches := luaRequireRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			edges = append(edges, ExtractedEdge{
				SourceName: filePath,
				TargetName: matches[1],
				Kind:       "imports",
				File:       filePath,
				Line:       lineNum,
			})
			continue
		}

		// Method (obj:method())
		if matches := luaMethodRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "method",
				Name:     matches[2],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: e.language,
			})
			continue
		}

		// Function
		if matches := luaFuncRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "function",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: e.language,
			})
		}
	}

	return nodes, edges
}

// ---------- Pascal/Delphi extraction ----------

var (
	pascalFuncRe  = regexp.MustCompile(`(?i)function\s+(\w+)\s*(?:\([^)]*\))?\s*:`)
	pascalProcRe  = regexp.MustCompile(`(?i)procedure\s+(\w+)\s*(?:\([^)]*\))?\s*;`)
	pascalClassRe = regexp.MustCompile(`(?i)(\w+)\s*=\s*class`)
	pascalUnitRe  = regexp.MustCompile(`(?i)unit\s+(\w+)\s*;`)
)

func (e *Extractor) extractPascal(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	lines := strings.Split(source, "\n")
	var nodes []ExtractedNode

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Unit
		if matches := pascalUnitRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "unit",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "pascal",
			})
			continue
		}

		// Class
		if matches := pascalClassRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "class",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "pascal",
			})
			continue
		}

		// Function
		if matches := pascalFuncRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "function",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "pascal",
			})
			continue
		}

		// Procedure
		if matches := pascalProcRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			nodes = append(nodes, ExtractedNode{
				Kind:     "procedure",
				Name:     matches[1],
				File:     filePath,
				Line:     lineNum,
				EndLine:  lineNum,
				Language: "pascal",
			})
		}
	}

	return nodes, nil
}

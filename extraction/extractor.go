package extraction

import (
	"regexp"
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
func (e *Extractor) Extract(source string, filePath string) ExtractResult {
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
	return promoteCallsToRefs(nodes, edges, filePath, e.language)
}

// promoteCallsToRefs moves call edges into UnresolvedReference so the
// orchestrator can same-file-link or park them as pending (step 2).
func promoteCallsToRefs(nodes []ExtractedNode, edges []ExtractedEdge, filePath, lang string) ExtractResult {
	out := ExtractResult{Nodes: nodes}
	// Map symbol name → def line (first wins) for FromLine stamping.
	defLine := make(map[string]int, len(nodes))
	for _, n := range nodes {
		if _, ok := defLine[n.Name]; !ok {
			defLine[n.Name] = n.Line
		}
	}
	for _, e := range edges {
		if e.Kind == "calls" {
			// Do NOT drop noisy names here: same-file link needs the ref first
			// (e.g. add/new/close). Noise is filtered only when parking cross-file
			// unknowns in parkUnresolved.
			fromLine := e.Line
			if dl, ok := defLine[e.SourceName]; ok {
				fromLine = dl
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

// ---------- Go extraction ----------

var (
	goFuncRe    = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?(\w+)\s*\(`)
	goTypeRe    = regexp.MustCompile(`^type\s+(\w+)\s+(struct|interface)\b`)
	goCallRe    = regexp.MustCompile(`\b(\w+)\s*\(`)
	goImportRe  = regexp.MustCompile(`"([^"]+)"`)
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

			// Extract function calls from body
			body := extractBody(lines, i, endLine)
			callMatches := goCallRe.FindAllStringSubmatch(body, -1)
			seen := map[string]bool{matches[1]: true}
			for _, m := range callMatches {
				if len(m) > 1 && !seen[m[1]] && !isGoKeyword(m[1]) {
					seen[m[1]] = true
					edges = append(edges, ExtractedEdge{
						SourceName: matches[1],
						TargetName: m[1],
						Kind:       "calls",
						File:       filePath,
						Line:       lineNum,
					})
				}
			}
			continue
		}
	}

	return nodes, edges
}

// ---------- TypeScript/JavaScript extraction ----------

var (
	jsFuncRe   = regexp.MustCompile(`(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*\(`)
	jsClassRe  = regexp.MustCompile(`(?:export\s+)?class\s+(\w+)`)
	jsMethodRe = regexp.MustCompile(`^\s+(?:async\s+)?(\w+)\s*\(`)
	jsCallRe   = regexp.MustCompile(`\b(\w+)\s*\(`)
	jsImportRe = regexp.MustCompile(`(?:from\s+['"]([^'"]+)['"]|require\s*\(\s*['"]([^'"]+)['"]\s*\))`)
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

			// Extract calls
			callMatches := jsCallRe.FindAllStringSubmatch(body, -1)
			seen := map[string]bool{matches[1]: true}
			for _, m := range callMatches {
				if len(m) > 1 && !seen[m[1]] && !isJSKeyword(m[1]) {
					seen[m[1]] = true
					edges = append(edges, ExtractedEdge{
						SourceName: matches[1],
						TargetName: m[1],
						Kind:       "calls",
						File:       filePath,
						Line:       lineNum,
					})
				}
			}
			continue
		}
	}

	return nodes, edges
}

// ---------- Python extraction ----------

var (
	pyFuncRe  = regexp.MustCompile(`^def\s+(\w+)\s*\(`)
	pyClassRe = regexp.MustCompile(`^class\s+(\w+)`)
	pyCallRe  = regexp.MustCompile(`\b(\w+)\s*\(`)
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

			// Extract calls
			callMatches := pyCallRe.FindAllStringSubmatch(body, -1)
			seen := map[string]bool{matches[1]: true}
			for _, m := range callMatches {
				if len(m) > 1 && !seen[m[1]] && !isPythonKeyword(m[1]) {
					seen[m[1]] = true
					edges = append(edges, ExtractedEdge{
						SourceName: matches[1],
						TargetName: m[1],
						Kind:       "calls",
						File:       filePath,
						Line:       lineNum,
					})
				}
			}
			continue
		}
	}

	return nodes, edges
}

// ---------- Rust extraction (regex; enough for use + fn + calls + metadata) ----------

var (
	rustFnRe     = regexp.MustCompile(`^(?:pub(?:\s*\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+(\w+)\s*(?:<[^>]*>)?\s*(\([^)]*\))\s*(?:->\s*([^{;]+))?`)
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

		if matches := rustFnRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			name := matches[1]
			params := strings.TrimSpace(matches[2])
			ret := strings.TrimSpace(matches[3])
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
			// calls inside body
			seen := map[string]bool{name: true}
			for _, m := range rustCallRe.FindAllStringSubmatch(body, -1) {
				if len(m) < 2 || seen[m[1]] || isRustKeyword(m[1]) {
					continue
				}
				seen[m[1]] = true
				edges = append(edges, ExtractedEdge{
					SourceName: name,
					TargetName: m[1],
					Kind:       "calls",
					File:       filePath,
					Line:       lineNum,
				})
			}
		}
	}
	return nodes, edges
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

// findBraceEnd finds the line where the matching closing brace is.
func findBraceEnd(lines []string, start int) int {
	depth := 0
	inString := false
	stringChar := byte(0)

	for i := start; i < len(lines) && i < start+500; i++ {
		for j := 0; j < len(lines[i]); j++ {
			ch := lines[i][j]
			if inString {
				if ch == '\\' {
					j++ // skip escaped char
					continue
				}
				if ch == stringChar {
					inString = false
				}
				continue
			}
			if ch == '"' || ch == '\'' || ch == '`' {
				inString = true
				stringChar = ch
				continue
			}
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
	}
	return start + 1
}

// findIndentEnd finds the end of an indented block (Python).
func findIndentEnd(lines []string, start int) int {
	if start+1 >= len(lines) {
		return start + 1
	}

	baseIndent := countIndent(lines[start+1])
	if baseIndent == 0 {
		return start + 1
	}

	for i := start + 2; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if countIndent(lines[i]) < baseIndent {
			return i
		}
	}
	return len(lines)
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
	objcMethodRe  = regexp.MustCompile(`^[-+]\s*\([^)]+\)\s*(\w+)`)
	objcClassRe   = regexp.MustCompile(`@interface\s+(\w+)`)
	objcImportRe  = regexp.MustCompile(`#import\s+[<"]([^>"]+)[>"]`)
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
	pascalFuncRe    = regexp.MustCompile(`(?i)function\s+(\w+)\s*(?:\([^)]*\))?\s*:`)
	pascalProcRe    = regexp.MustCompile(`(?i)procedure\s+(\w+)\s*(?:\([^)]*\))?\s*;`)
	pascalClassRe   = regexp.MustCompile(`(?i)(\w+)\s*=\s*class`)
	pascalUnitRe    = regexp.MustCompile(`(?i)unit\s+(\w+)\s*;`)
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

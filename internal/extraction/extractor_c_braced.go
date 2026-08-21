package extraction

import (
	"regexp"
	"strings"
)

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

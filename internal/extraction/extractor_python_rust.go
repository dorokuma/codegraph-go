package extraction

import (
	"regexp"
	"strings"
)

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

package extraction

import (
	"regexp"
	"strings"
)

// ExtractedNode represents a symbol found in source code.
type ExtractedNode struct {
	Kind     string // function, class, method, struct, interface, type, variable, constant
	Name     string
	File     string
	Line     int
	EndLine  int
	Body     string
	Language string
}

// ExtractedEdge represents a relationship found in source code.
type ExtractedEdge struct {
	SourceName string
	TargetName string
	Kind       string // calls, imports, extends, implements
	File       string
	Line       int
}

// Extractor extracts symbols and edges from source code.
type Extractor struct {
	language string
}

// NewExtractor creates an extractor for the given language.
func NewExtractor(language string) *Extractor {
	return &Extractor{language: language}
}

// Extract parses the source code and returns nodes and edges.
func (e *Extractor) Extract(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	switch e.language {
	case "go":
		return e.extractGo(source, filePath)
	case "typescript", "javascript":
		return e.extractJS(source, filePath)
	case "python":
		return e.extractPython(source, filePath)
	case "objective-c":
		return e.extractObjC(source, filePath)
	case "svelte", "vue", "astro":
		return e.extractFrontendComponent(source, filePath)
	case "liquid":
		return e.extractLiquid(source, filePath)
	case "luau":
		return e.extractLua(source, filePath)
	case "pascal":
		return e.extractPascal(source, filePath)
	default:
		return e.extractGeneric(source, filePath)
	}
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

// ---------- Generic extraction (fallback) ----------

var (
	genericFuncRe  = regexp.MustCompile(`(?:function|def|fn|func)\s+(\w+)\s*\(`)
	genericClassRe = regexp.MustCompile(`(?:class|struct|interface)\s+(\w+)`)
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

func isGoKeyword(s string) bool {
	goKeywords := map[string]bool{
		"if": true, "else": true, "for": true, "range": true, "switch": true,
		"case": true, "default": true, "select": true, "defer": true, "go": true,
		"return": true, "break": true, "continue": true, "fallthrough": true,
		"var": true, "const": true, "type": true, "func": true, "struct": true,
		"interface": true, "map": true, "chan": true, "package": true, "import": true,
		"new": true, "make": true, "append": true, "len": true, "cap": true,
		"delete": true, "copy": true, "print": true, "println": true, "panic": true,
		"recover": true, "close": true, "true": true, "false": true, "nil": true,
		"int": true, "string": true, "bool": true, "float64": true, "error": true,
		"byte": true, "rune": true, "uint": true, "int64": true, "any": true,
	}
	return goKeywords[s]
}

func isJSKeyword(s string) bool {
	jsKeywords := map[string]bool{
		"if": true, "else": true, "for": true, "while": true, "do": true,
		"switch": true, "case": true, "default": true, "break": true, "continue": true,
		"return": true, "throw": true, "try": true, "catch": true, "finally": true,
		"var": true, "let": true, "const": true, "function": true, "class": true,
		"new": true, "delete": true, "typeof": true, "instanceof": true, "void": true,
		"this": true, "super": true, "import": true, "export": true, "from": true,
		"async": true, "await": true, "yield": true, "null": true, "undefined": true,
		"true": true, "false": true, "console": true, "require": true, "module": true,
		"Promise": true, "Array": true, "Object": true, "String": true, "Number": true,
		"Boolean": true, "Map": true, "Set": true, "Date": true, "Math": true,
		"JSON": true, "Error": true, "RegExp": true, "Symbol": true, "Proxy": true,
	}
	return jsKeywords[s]
}

func isPythonKeyword(s string) bool {
	pyKeywords := map[string]bool{
		"if": true, "elif": true, "else": true, "for": true, "while": true,
		"break": true, "continue": true, "pass": true, "return": true, "yield": true,
		"raise": true, "try": true, "except": true, "finally": true, "with": true,
		"as": true, "def": true, "class": true, "lambda": true, "and": true,
		"or": true, "not": true, "is": true, "in": true, "True": true,
		"False": true, "None": true, "self": true, "cls": true, "super": true,
		"print": true, "len": true, "range": true, "type": true, "int": true,
		"str": true, "float": true, "list": true, "dict": true, "set": true,
		"tuple": true, "bool": true, "import": true, "from": true, "global": true,
		"nonlocal": true, "assert": true, "del": true, "exec": true,
	}
	return pyKeywords[s]
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

// ---------- Frontend component extraction (Svelte, Vue, Astro) ----------

var (
	// Extract script content from <script> tags
	scriptTagRe = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
)

func (e *Extractor) extractFrontendComponent(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	// Extract script content
	matches := scriptTagRe.FindStringSubmatch(source)
	if len(matches) < 2 {
		return nil, nil
	}

	scriptContent := matches[1]

	// Use JS extractor on the script content
	ext := NewExtractor("javascript")
	nodes, edges := ext.Extract(scriptContent, filePath)

	// Update language
	for i := range nodes {
		nodes[i].Language = e.language
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

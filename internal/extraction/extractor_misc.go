package extraction

import (
	"regexp"
	"strings"
)

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

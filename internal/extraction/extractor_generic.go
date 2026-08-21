package extraction

import (
	"regexp"
	"strings"
)

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

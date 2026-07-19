package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolCalleesBodyFallback is the legacy rg + brace-matching path used when
// the call graph has no edges for the symbol yet.
func (s *server) toolCalleesBodyFallback(ctx context.Context, args nameArgs) (*mcp.CallToolResult, any, error) {
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	// Guard against rg hanging on large trees or named pipes.
	rgCtx, rgCancel := context.WithTimeout(ctx, 10*time.Second)
	defer rgCancel()
	quoted := regexp.QuoteMeta(args.Name)
	defPattern := fmt.Sprintf(`(func\s+(\([^)]*\)\s*)?|def |defn |function |async function |fn |class )%s\b`, quoted)
	rgDef := exec.CommandContext(rgCtx, "rg",
		"--line-number", "--no-heading", "--color=never",
		"--max-count=20",
		"-e", defPattern, root)
	defOut, err := rgDef.Output()
	if err != nil || len(bytes.TrimSpace(defOut)) == 0 {
		fallbackPattern := fmt.Sprintf(`\b%s\s*\(`, quoted)
		rgDefFallback := exec.CommandContext(rgCtx, "rg",
			"--line-number", "--no-heading", "--color=never",
			"--max-count=20",
			"-e", fallbackPattern, root)
		defOut, err = rgDefFallback.Output()
		if err != nil || len(bytes.TrimSpace(defOut)) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "no definitions found for " + args.Name}},
			}, nil, nil
		}
	}

	type defMatch struct {
		file string
		line int
	}
	var defs []defMatch
	for _, line := range strings.Split(strings.TrimSpace(string(defOut)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		ln, err := strconv.Atoi(parts[1])
		if err != nil || ln <= 0 {
			continue
		}
		defs = append(defs, defMatch{file: parts[0], line: ln})
	}
	if len(defs) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no definitions found for " + args.Name}},
		}, nil, nil
	}

	callRe := regexp.MustCompile(`\b([a-zA-Z_]\w*)\s*\(`)
	controlFlow := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "case": true,
		"return": true, "defer": true, "go": true, "select": true,
		"range": true, "catch": true, "try": true, "finally": true,
		"elif": true, "except": true, "with": true, "continue": true, "break": true,
		"import": true, "from": true, "async": true, "await": true, "yield": true,
		"func": true, "function": true, "var": true, "let": true, "const": true,
		"type": true, "struct": true, "interface": true, "map": true, "chan": true,
		"new": true, "make": true, "append": true, "len": true, "cap": true,
		"delete": true, "copy": true, "print": true, "println": true, "panic": true,
		"recover": true, "close": true,
		"this": true, "super": true, "nil": true, "null": true, "true": true, "false": true,
		"int": true, "string": true, "bool": true, "float64": true, "error": true,
		"byte": true, "rune": true, "uint": true, "int64": true,
		"uint8": true, "uint16": true, "uint32": true, "uint64": true,
		"int8": true, "int16": true, "int32": true,
		"float32": true,
		"complex64": true, "complex128": true, "uintptr": true,
	}

	type callInfo struct {
		callee string
		file   string
		line   int
	}
	var allCalls []callInfo
	for _, d := range defs {
		if len(allCalls) >= args.MaxResults {
			break
		}
		lines, err := readLines(d.file)
		if err != nil {
			continue
		}
		seen := make(map[string]bool)

		bodyStart := d.line - 1
		if bodyStart >= len(lines) {
			continue
		}

		hasBrace := false
		searchLines := 3
		if bodyStart+searchLines > len(lines) {
			searchLines = len(lines) - bodyStart
		}
		for i := 0; i < searchLines; i++ {
			cleanLine := stripStringsAndComments(lines[bodyStart+i])
			if strings.Contains(cleanLine, "{") {
				hasBrace = true
				break
			}
		}
		if !hasBrace {
			if strings.HasSuffix(d.file, ".py") {
				colonLine := bodyStart
				for colonLine < len(lines) && colonLine <= bodyStart+5 {
					if strings.Contains(strings.TrimSpace(lines[colonLine]), ":") {
						break
					}
					colonLine++
				}
				if colonLine >= len(lines) || colonLine > bodyStart+5 {
					continue
				}

				firstBodyLine := -1
				for i := colonLine + 1; i < len(lines); i++ {
					trimmed := strings.TrimSpace(lines[i])
					if trimmed == "" || strings.HasPrefix(trimmed, "#") {
						continue
					}
					firstBodyLine = i
					break
				}
				if firstBodyLine == -1 {
					continue
				}

				baseIndent := countLeadingSpaces(lines[firstBodyLine])
				if baseIndent == 0 {
					continue
				}

				bodyEnd := firstBodyLine
				maxScan := bodyStart + 500
				if maxScan > len(lines) {
					maxScan = len(lines)
				}
				for i := firstBodyLine + 1; i < maxScan; i++ {
					trimmed := strings.TrimSpace(lines[i])
					if trimmed == "" || strings.HasPrefix(trimmed, "#") {
						continue
					}
					if countLeadingSpaces(lines[i]) < baseIndent {
						break
					}
					bodyEnd = i
				}

				for i := bodyStart; i <= bodyEnd && i < len(lines) && len(allCalls) < args.MaxResults; i++ {
					line := lines[i]
					clean := stripStringsAndComments(line)
					matches := callRe.FindAllStringSubmatch(clean, -1)
					for _, m := range matches {
						name := m[1]
						if name == args.Name || controlFlow[name] || seen[name] {
							continue
						}
						seen[name] = true
						allCalls = append(allCalls, callInfo{
							callee: name,
							file:   d.file,
							line:   i + 1,
						})
					}
				}
				continue
			}
			continue
		}

		braceCount := 0
		foundOpen := false
		bodyEnd := bodyStart
		maxScan := bodyStart + 500
		if maxScan > len(lines) {
			maxScan = len(lines)
		}
		for i := bodyStart; i < maxScan; i++ {
			bodyEnd = i
			line := lines[i]
			inString := false
			stringChar := byte(0)
			skip := false
			for j := 0; j < len(line); j++ {
				ch := line[j]
				if skip {
					skip = false
					continue
				}
				if !inString && ch == '/' && j+1 < len(line) {
					if line[j+1] == '/' {
						break
					}
					if line[j+1] == '*' {
						inString = true
						stringChar = 0
						j++
						continue
					}
				}
				if inString {
					if stringChar == 0 {
						if ch == '*' && j+1 < len(line) && line[j+1] == '/' {
							inString = false
							j++
						}
					} else {
						if stringChar != '`' {
							if ch == '\\' {
								skip = true
								continue
							}
						}
						if ch == stringChar {
							inString = false
						}
					}
					continue
				}
				if ch == '"' || ch == '\'' || ch == '`' {
					inString = true
					stringChar = ch
					continue
				}
				if ch == '{' {
					foundOpen = true
					braceCount++
				}
				if ch == '}' {
					braceCount--
				}
			}
			if foundOpen && braceCount == 0 {
				break
			}
		}

		for i := bodyStart; i <= bodyEnd && i < len(lines) && len(allCalls) < args.MaxResults; i++ {
			line := lines[i]
			clean := stripStringsAndComments(line)
			matches := callRe.FindAllStringSubmatch(clean, -1)
			for _, m := range matches {
				name := m[1]
				if name == args.Name || controlFlow[name] || seen[name] {
					continue
				}
				seen[name] = true
				allCalls = append(allCalls, callInfo{
					callee: name,
					file:   d.file,
					line:   i + 1,
				})
			}
		}
	}

	if len(allCalls) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: args.Name + " calls no external functions (or body could not be parsed)"}},
		}, nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Functions called by %s (%d callees):\n", args.Name, len(allCalls))
	currentFile := ""
	for _, c := range allCalls {
		if c.file != currentFile {
			currentFile = c.file
			rel, _ := filepath.Rel(s.workdir, c.file)
			if rel == "" {
				rel = c.file
			}
			fmt.Fprintf(&b, "\n%s:\n", rel)
		}
		fmt.Fprintf(&b, "  %5d  %s()\n", c.line, c.callee)
	}
	if len(allCalls) >= args.MaxResults {
		fmt.Fprintf(&b, "\n... (max %d, truncated)", args.MaxResults)
	}

	out := "# Callees of " + args.Name + " (body-parse fallback — index had no call edges)\n" + b.String()
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: truncateOutput(out, defaultOutputChars)}},
	}, nil, nil
}


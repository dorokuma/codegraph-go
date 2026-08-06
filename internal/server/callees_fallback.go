package server

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callRe matches a bare identifier followed by optional spaces and '('.
// Compiled once at package init — used by toolCalleesBodyFallback to discover
// callee names inside function bodies.
var callRe = regexp.MustCompile(`\b([a-zA-Z_]\w*)\s*\(`)

// toolCalleesBodyFallback is the legacy rg + brace-matching path used when
// the call graph has no edges for the symbol yet.
func (s *Server) toolCalleesBodyFallback(ctx context.Context, root string, database *db.DB, args nameArgs) (*mcp.CallToolResult, any, error) {
	// Guard against rg hanging on large trees or named pipes.
	rgCtx, rgCancel := context.WithTimeout(ctx, 10*time.Second)
	defer rgCancel()
	// Safety: QuoteMeta escapes all regex metacharacters so args.Name cannot
	// inject arbitrary patterns into the rg -e argument.
	quoted := regexp.QuoteMeta(args.Name)
	defPattern := fmt.Sprintf(`(func\s+(\([^)]*\)\s*)?|def |defn |function |async function |fn |class )%s\b`, quoted)
	rgDef := exec.CommandContext(rgCtx, "rg",
		"--line-number", "--no-heading", "--color=never",
		"--max-count=20",
		"-e", defPattern, root)
	defOut, err := rgDef.Output()
	if err != nil || len(bytes.TrimSpace(defOut)) == 0 {
		// err != nil with a non-exit-1 code means rg itself is broken (not
		// merely "no match") — surface the error instead of masking it with a
		// misleading "no definitions" answer or a pointless fallback run.
		if err != nil {
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
				return nil, nil, fmt.Errorf("rg definitions: %w", err)
			}
		}
		// Fallback uses an independent context — primary may have exhausted its timeout.
		fallbackCtx, fallbackCancel := context.WithTimeout(ctx, 10*time.Second)
		defer fallbackCancel()
		fallbackPattern := fmt.Sprintf(`\b%s\s*\(`, quoted)
		rgDefFallback := exec.CommandContext(fallbackCtx, "rg",
			"--line-number", "--no-heading", "--color=never",
			"--max-count=20",
			"-e", fallbackPattern, root)
		defOut, err = rgDefFallback.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
				return nil, nil, fmt.Errorf("rg definitions fallback: %w", err)
			}
		}
		if len(bytes.TrimSpace(defOut)) == 0 {
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
		"float32":   true,
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
		// d.file comes from rg output: it may be (or pass through) a symlink
		// pointing outside the workspace. resolvePathIn enforces the realpath
		// + escape jail before the file is read, so a symlink cannot smuggle
		// reads outside the workspace root (H3).
		p, err := s.resolvePathIn(root, d.file)
		if err != nil {
			continue
		}
		lines, err := readLines(p)
		if err != nil {
			continue
		}
		// seen is per-def — each function body is scanned independently.
		// A symbol called by multiple definitions will appear under each def,
		// which matches the caller-facing UX ("what does THIS definition call?").
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

	// B5/W6: output one entry per callee with the callee name, and prefer the
	// callee's definition position from the index (same-file/scope node first,
	// then any node with that name). Only when no definition exists do we emit
	// the call-site position, explicitly labeled as fallback/call-site
	// semantics so it cannot be confused with graph-path results. The leading
	// file:line token stays shape-compatible with the graph path.
	type defLoc struct {
		file string
		line int
	}
	// Cache resolution per (callee, def file) — the same-file preference
	// depends on which definition body the call was found in.
	defCache := make(map[string]defLoc)
	notFound := make(map[string]bool)
	resolveDef := func(callee, defFile string) (defLoc, bool) {
		key := callee + "|" + defFile
		if loc, ok := defCache[key]; ok {
			return loc, true
		}
		if notFound[key] {
			return defLoc{}, false
		}
		file, line, ok := s.calleeDefLocation(ctx, database, root, callee, defFile)
		if ok {
			defCache[key] = defLoc{file: file, line: line}
		} else {
			notFound[key] = true
		}
		return defLoc{file: file, line: line}, ok
	}

	var b strings.Builder
	for _, c := range allCalls {
		if loc, ok := resolveDef(c.callee, c.file); ok {
			fmt.Fprintf(&b, "%s:%d  %s\n", db.RelPath(root, loc.file), loc.line, c.callee)
		} else {
			fmt.Fprintf(&b, "%s:%d  %s  (call site, fallback)\n", db.RelPath(root, c.file), c.line, c.callee)
		}
	}
	if len(allCalls) >= args.MaxResults {
		fmt.Fprintf(&b, "... (max %d)\n", args.MaxResults)
	}

	out := b.String()
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: truncateOutput(out, defaultOutputChars)}},
	}, nil, nil
}

// calleeDefLocation resolves a callee name to a definition node in the index
// (B5). Prefers a node in the same file as the caller's definition (target
// file/scope); falls back to any indexed node with that name. Returns the
// node's storage path + line, or ok=false when the index has no definition.
func (s *Server) calleeDefLocation(ctx context.Context, database *db.DB, root, name, defFile string) (file string, line int, ok bool) {
	nodes, err := database.GetNodeByNameContext(ctx, name)
	if err != nil || len(nodes) == 0 {
		return "", 0, false
	}
	sameFileKey := db.StoragePath(root, defFile)
	var anyFile string
	var anyLine int
	for i := range nodes {
		n := &nodes[i]
		if n.Kind == db.KindFile || n.Kind == "module" || n.File == "" {
			continue
		}
		if anyFile == "" {
			anyFile, anyLine = n.File, n.Line
		}
		if n.File == sameFileKey {
			return n.File, n.Line, true
		}
	}
	if anyFile != "" {
		return anyFile, anyLine, true
	}
	return "", 0, false
}

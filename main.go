// codegraph-go: a Go MCP server that mimics the official colbymchenry/codegraph
// tool surface (codegraph_search, codegraph_files, codegraph_context,
// codegraph_explore, codegraph_status, codegraph_callees, codegraph_callers,
// codegraph_trace, codegraph_impact) so it can drop in as a [[plugins]] entry
// in reasonix.toml under the name "codegraph" and silence the
// "(built-in, not installed)" stub.
//
// This is intentionally a thin layer: file glob + ripgrep + line-window reads.
// It does NOT do real AST analysis; the goal is to match tool *shape*, not to
// rival the official code search quality.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxFileSize = 10 * 1024 * 1024 // 10 MB — files larger than this are skipped in LOC stats

type server struct {
	workdir     string
	mu          sync.Mutex
	lastScan    time.Time
	cachedLoc   int
	cachedFiles int
}

func main() {
	var workdir string
	flag.StringVar(&workdir, "workdir", "", "workspace root (default: cwd)")
	flag.Parse()
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatalf("cannot determine current directory: %v", err)
		}
		workdir = wd
	}
	absWd, err := filepath.Abs(workdir)
	if err != nil {
		log.Fatalf("bad workdir %q: %v", workdir, err)
	}
	workdir = absWd
	log.Printf("codegraph-go starting, workdir=%s", workdir)

	s := &server{workdir: workdir}

	srv := mcp.NewServer(&mcp.Implementation{Name: "codegraph-go", Version: "0.1.0"}, nil)

	// 9 tools mirroring the official codegraph surface (string-grepped from
	// /usr/local/bin/reasonix: codegraph_search, codegraph_files,
	// codegraph_context, codegraph_explore, codegraph_status,
	// codegraph_callees, codegraph_callers, codegraph_trace, codegraph_impact)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_search",
		Description: "ripgrep-style text/regex search across the workspace. Returns matching file paths with line numbers.",
	}, s.toolSearch)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_files",
		Description: "List files in the workspace matching a glob pattern (supports ** recursion). Uses ripgrep (respects .gitignore).",
	}, s.toolFiles)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_context",
		Description: "Read a window of lines around a given file:line position. Use after codegraph_search to grab surrounding code.",
	}, s.toolContext)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_explore",
		Description: "List candidate entry points: top-level directories, README files, package manifests. Cheap first step on a new repo.",
	}, s.toolExplore)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_status",
		Description: "Report server version, workspace root, file count, and total LOC under the workspace.",
	}, s.toolStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_callees",
		Description: "List functions that <symbol> calls — extracts function names from the body of the matched definition.",
	}, s.toolCallees)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_callers",
		Description: "Find references (call sites) to `name` across the workspace.",
	}, s.toolCallers)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_trace",
		Description: "Grep for `name` across the workspace; depth=1 adds 5 lines of context per match.",
	}, s.toolTrace)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "codegraph_impact",
		Description: "Reverse impact: which files reference `name`? Returns a sorted file list with match counts.",
	}, s.toolImpact)

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

// ---------- tool implementations ----------

type searchArgs struct {
	Pattern    string `json:"pattern"      jsonschema:"regex or literal pattern (ripgrep syntax)"`
	Path       string `json:"path,omitempty" jsonschema:"optional subdirectory under workspace,optional"`
	Glob       string `json:"glob,omitempty" jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"cap on number of matches per file (default 200),optional"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"case-insensitive search,optional"`
}

func (s *server) toolSearch(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	// 30s timeout to prevent rg from hanging on large repos
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Pattern == "" {
		return nil, nil, fmt.Errorf("pattern is required")
	}
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	if args.MaxResults == 0 {
		args.MaxResults = 200
	}
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		fmt.Sprintf("--max-count=%d", args.MaxResults),
	)
	if args.IgnoreCase {
		rg.Args = append(rg.Args, "-i")
	}
	if args.Glob != "" {
		rg.Args = append(rg.Args, "--glob", args.Glob)
	}
	rg.Args = append(rg.Args, args.Pattern, root)
	stdout, err := rg.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := rg.Start(); err != nil {
		return nil, nil, err
	}

	var buf bytes.Buffer
	limit := 2 * 1024 * 1024 // 2 MB limit to prevent OOM
	tmp := make([]byte, 8192)
	for buf.Len() < limit {
		n, err := stdout.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	_ = rg.Process.Kill()
	_ = rg.Wait()

	out := buf.Bytes()
	if len(out) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no matches"}},
		}, nil, nil
	}
	// truncate to keep token cost low — safe at rune boundary
	text := string(out)
	if len(text) > 50_000 {
		// find the last valid rune boundary at or before 50_000
		truncAt := 50_000
		for truncAt > 0 && !utf8.ValidString(text[:truncAt]) {
			truncAt--
		}
		text = text[:truncAt] + "\n... (truncated)"
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

type filesArgs struct {
	Pattern string `json:"pattern,omitempty" jsonschema:"glob pattern relative to workspace, e.g. \"src/**/*.go\",optional"`
	Max     int    `json:"max,omitempty"     jsonschema:"cap (default 500),optional"`
}

func (s *server) toolFiles(ctx context.Context, _ *mcp.CallToolRequest, args filesArgs) (*mcp.CallToolResult, any, error) {
	// 30s timeout to prevent rg from hanging on large repos
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pattern := args.Pattern
	if pattern == "" {
		pattern = "**/*"
	}
	if args.Max == 0 {
		args.Max = 500
	}
	root := s.workdir
	fullPath := filepath.Join(root, pattern)
	if info, err := os.Stat(fullPath); err == nil && info.IsDir() {
		if strings.HasSuffix(pattern, "/") {
			pattern = pattern + "**/*"
		} else {
			pattern = pattern + "/**/*"
		}
	}
	// Use rg --files for ** glob support + .gitignore awareness
	rg := exec.CommandContext(ctx, "rg", "--files", "-g", pattern, root)
	out, err := rg.Output()
	if err != nil {
		// rg exits 1 when no files match
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no files matched"}},
		}, nil, nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > args.Max {
		lines = lines[:args.Max]
	}
	// strip root prefix for readability
	var b strings.Builder
	for _, l := range lines {
		rel, err := filepath.Rel(root, l)
		if err == nil {
			fmt.Fprintln(&b, rel)
		}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}, nil, nil
}

type contextArgs struct {
	File   string `json:"file"               jsonschema:"file path (absolute or workspace-relative)"`
	Line   int    `json:"line"               jsonschema:"1-based line number to center on"`
	Before int    `json:"before,omitempty"   jsonschema:"lines of context before (default 20),optional"`
	After  int    `json:"after,omitempty"    jsonschema:"lines of context after (default 20),optional"`
}

func (s *server) toolContext(ctx context.Context, _ *mcp.CallToolRequest, args contextArgs) (*mcp.CallToolResult, any, error) {
	if args.File == "" || args.Line <= 0 {
		return nil, nil, fmt.Errorf("file and line (>0) are required")
	}
	if args.Before < 0 {
		args.Before = 0
	}
	if args.After < 0 {
		args.After = 0
	}
	// Resolve to absolute path and verify it's under workspace root
	fullPath, err := s.resolvePath(args.File)
	if err != nil {
		return nil, nil, err
	}
	all, err := readLines(fullPath)
	if err != nil {
		return nil, nil, err
	}
	start := args.Line - 1 - args.Before
	if start < 0 {
		start = 0
	}
	end := args.Line - 1 + args.After + 1
	if end > len(all) {
		end = len(all)
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		marker := "  "
		if i+1 == args.Line {
			marker = ">>"
		}
		fmt.Fprintf(&b, "%s%5d  %s\n", marker, i+1, all[i])
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}, nil, nil
}

type exploreArgs struct {
	Max int `json:"max,omitempty" jsonschema:"cap on number of entries (default 30),optional"`
}

func (s *server) toolExplore(ctx context.Context, _ *mcp.CallToolRequest, args exploreArgs) (*mcp.CallToolResult, any, error) {
	if args.Max == 0 {
		args.Max = 30
	}
	// cheap "where do I start" view: top-level dirs + README + manifests
	entries, err := os.ReadDir(s.workdir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading workspace: %w", err)
	}
	var b strings.Builder
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fmt.Fprintf(&b, "%s/\n", e.Name())
	}
	// README hunt (1 level deep)
	rg := exec.CommandContext(ctx, "rg", "--files", "-g", "README*", "-g", "*.md", s.workdir)
	if out, err := rg.Output(); err == nil {
		text := strings.TrimSpace(string(out))
		if text != "" {
			fmt.Fprintln(&b, "\n# docs")
			lines := strings.SplitN(text, "\n", args.Max+1)
			if len(lines) > args.Max {
				lines = lines[:args.Max]
			}
			for _, l := range lines {
				if rel, err := filepath.Rel(s.workdir, l); err == nil {
					fmt.Fprintln(&b, rel)
				}
			}
		}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}, nil, nil
}

type statusArgs struct{}

func (s *server) toolStatus(ctx context.Context, _ *mcp.CallToolRequest, _ statusArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.lastScan.IsZero() && time.Since(s.lastScan) < 5*time.Minute {
		return s.formatStatusResult(s.cachedFiles, s.cachedLoc)
	}

	files := 0
	loc := 0
	filepath.Walk(s.workdir, func(walkPath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if err == nil && info.IsDir() {
				name := info.Name()
				if strings.HasPrefix(name, ".") || name == "node_modules" || name == "go" || name == "rtk" || name == "qdrant_storage" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		files++
		if !isProbablyBinary(info.Name()) && info.Size() <= maxFileSize {
			if count, err := countFileLines(walkPath); err == nil {
				loc += count
			}
		}
		return nil
	})

	s.cachedFiles = files
	s.cachedLoc = loc
	s.lastScan = time.Now()

	return s.formatStatusResult(files, loc)
}

func countFileLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, 32*1024)
	count := 0
	lineSep := []byte{'\n'}

	for {
		c, err := f.Read(buf)
		if c > 0 {
			count += bytes.Count(buf[:c], lineSep)
		}
		if err != nil {
			break
		}
	}
	return count, nil
}

func (s *server) formatStatusResult(files, loc int) (*mcp.CallToolResult, any, error) {
	out := map[string]any{
		"version":   "0.1.0",
		"workspace": s.workdir,
		"files":     files,
		"loc":       loc,
		"server":    "codegraph-go",
		"note":      "implementation: grep + line reads (no AST); drop-in for reasonix.toml [[plugins]] name=codegraph",
	}
	js, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(js)}},
	}, nil, nil
}

type nameArgs struct {
	Name       string `json:"name"                 jsonschema:"symbol name to look for"`
	Path       string `json:"path,omitempty"        jsonschema:"optional subdirectory under workspace,optional"`
	Glob       string `json:"glob,omitempty"        jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"cap (default 100),optional"`
}

func (s *server) toolCallees(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	// 30s timeout to prevent rg from hanging on large repos
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = 100
	}
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	quoted := regexp.QuoteMeta(args.Name)

	// Step 1: find definition locations (function/method definitions)
	defPattern := fmt.Sprintf(`(func\s+(\([^)]*\)\s*)?|def |defn |function |async function |fn |class )%s\b`, quoted)
	rgDef := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		"--max-count=20",
		"-e", defPattern, root)
	defOut, err := rgDef.Output()
	if err != nil || len(bytes.TrimSpace(defOut)) == 0 {
		// Fallback: broader pattern matching any symbol followed by '('
		// catches C/C++/Java/TS/JS/etc. definitions the first pattern misses
		fallbackPattern := fmt.Sprintf(`\b%s\s*\(`, quoted)
		rgDefFallback := exec.CommandContext(ctx, "rg",
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

	// Parse file:line from matches
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

	// Step 2+3: read each definition body and extract function calls via brace matching
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

		bodyStart := d.line - 1 // 0-indexed
		if bodyStart >= len(lines) {
			continue
		}

		// Find function body with brace matching (max 300 lines).
		// First, check if there's an open brace '{' near the definition to avoid scanning non-function declarations.
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
			// For Python (.py) files, use indentation-based body detection instead of brace matching
			if strings.HasSuffix(d.file, ".py") {
				// Find the line containing ':' (the def statement may span multiple lines)
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

				// Skip blank / comment lines after def, find first body line
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
					// no indentation after def — likely a single-line stub
					continue
				}

				// Scan body until indentation drops below baseIndent (or max scan lines)
				bodyEnd := firstBodyLine
				maxScan := bodyStart + 500 // increased from 300 to support larger function bodies
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

				// Extract function calls from the body (same logic as the brace-based path)
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

		// Skip braces inside string literals and comments for better accuracy.
		braceCount := 0
		foundOpen := false
		bodyEnd := bodyStart
		maxScan := bodyStart + 500 // increased from 300 to support larger function bodies
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
					// single-line comment — rest of line is ignored
					if line[j+1] == '/' {
						break
					}
					// possible start of block comment
					if line[j+1] == '*' {
						inString = true
						stringChar = 0 // mark as block-comment mode
						j++
						continue
					}
				}
				if inString {
					if stringChar == 0 {
						// block comment mode
						if ch == '*' && j+1 < len(line) && line[j+1] == '/' {
							inString = false
							j++
						}
					} else {
						// backtick strings (raw) don't process escape sequences
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
				// not in string/comment
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

		// Extract function calls from the body (skip strings/comments)
		for i := bodyStart; i <= bodyEnd && i < len(lines) && len(allCalls) < args.MaxResults; i++ {
			line := lines[i]
			// Strip single-line comments and string contents before matching
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

	// Format output: group by file
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

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}, nil, nil
}

func (s *server) toolCallers(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	// 30s timeout to prevent rg from hanging on large repos
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = 100
	}
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	// word-boundary regex
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		fmt.Sprintf("--max-count=%d", args.MaxResults),
		"-w", args.Name, root)
	if args.Glob != "" {
		rg.Args = append(rg.Args, "--glob", args.Glob)
	}
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no references found"}}}, nil, nil
	}
	// Filter out false positives (comments/strings) and definition lines.
	quoted := regexp.QuoteMeta(args.Name)
	defRe := regexp.MustCompile(`(func\s+(\([^)]*\)\s*)?|def\s+|function\s+|class\s+|fn\s+)` + quoted + `\b`)
	var filtered []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		text := parts[2]
		cleaned := stripStringsAndComments(text)
		if !strings.Contains(cleaned, args.Name) {
			// Only present in string literals or comments — skip
			continue
		}
		if defRe.MatchString(cleaned) {
			// This is a definition, not a call site — skip
			continue
		}
		filtered = append(filtered, line)
	}
	if len(filtered) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no references found"}}}, nil, nil
	}
	result := strings.Join(filtered, "\n")
	if len(result) > 50_000 {
		truncAt := 50_000
		for truncAt > 0 && !utf8.ValidString(result[:truncAt]) {
			truncAt--
		}
		result = result[:truncAt] + "\n... (truncated)"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

type traceArgs struct {
	Name  string `json:"name"`
	Depth int    `json:"depth,omitempty" jsonschema:"follow-up grep depth, 0 or 1 (default 0),optional"`
}

func (s *server) toolTrace(ctx context.Context, _ *mcp.CallToolRequest, args traceArgs) (*mcp.CallToolResult, any, error) {
	// 30s timeout to prevent rg from hanging on large repos
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.Depth > 1 {
		args.Depth = 1
	}
	root := s.workdir
	rgArgs := []string{"--line-number", "--no-heading", "--color=never", "--max-count=100"}
	if args.Depth == 1 {
		rgArgs = append(rgArgs, "-C", "5") // 5 lines of surrounding context
	}
	rgArgs = append(rgArgs, "-w", args.Name, root)
	rg := exec.CommandContext(ctx, "rg", rgArgs...)
	out, _ := rg.Output()
	result := string(out)
	if len(result) > 50_000 {
		truncAt := 50_000
		for truncAt > 0 && !utf8.ValidString(result[:truncAt]) {
			truncAt--
		}
		result = result[:truncAt] + "\n... (truncated)"
	}
	if result == "" {
		result = "no matches found for " + args.Name
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

func (s *server) toolImpact(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	// 30s timeout to prevent rg from hanging on large repos
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = 100
	}
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		"-c", // count per file
		"-w", args.Name, root)
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no files reference " + args.Name}}}, nil, nil
	}
	result := string(out)
	if len(result) > 50_000 {
		truncAt := 50_000
		for truncAt > 0 && !utf8.ValidString(result[:truncAt]) {
			truncAt--
		}
		result = result[:truncAt] + "\n... (truncated)"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

// ---------- helpers ----------

// resolvePath converts a user-supplied path into an absolute path within the workspace.
// It guarantees the result is inside s.workdir, preventing path traversal.
func (s *server) resolvePath(p string) (string, error) {
	if p == "" {
		return s.workdir, nil
	}
	var target string
	if filepath.IsAbs(p) {
		target = filepath.Clean(p)
	} else {
		target = filepath.Clean(filepath.Join(s.workdir, p))
	}
	if target != s.workdir && !strings.HasPrefix(target, s.workdir+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside workspace %q", p, s.workdir)
	}
	return target, nil
}

func readLines(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 10*1024*1024 {
		return nil, fmt.Errorf("file %q is too large (> 10MB)", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	return strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), nil
}

func isProbablyBinary(name string) bool {
	for _, ext := range []string{
		".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".bmp",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".zip", ".tar", ".gz", ".bz2", ".xz", ".zst", ".7z", ".rar",
		".exe", ".bin", ".dll", ".so", ".dylib", ".wasm",
		".o", ".a", ".lib", ".obj",
		".mp3", ".mp4", ".avi", ".mov", ".wav", ".flac", ".ogg",
		".ttf", ".otf", ".woff", ".woff2",
		".pyc", ".pyo", ".class", ".jar",
		".db", ".sqlite", ".sqlite3",
		".iso", ".dmg", ".img",
	} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			return true
		}
	}
	return false
}

// stripStringsAndComments replaces string literals and comments with spaces,
// so regex matching on the result doesn't falsely match inside them.
func stripStringsAndComments(line string) string {
	var out strings.Builder
	out.Grow(len(line))
	inString := false
	stringChar := byte(0)
	skip := false
	for j := 0; j < len(line); j++ {
		ch := line[j]
		if skip {
			skip = false
			out.WriteByte(' ')
			continue
		}
		if !inString && ch == '/' && j+1 < len(line) {
			if line[j+1] == '/' {
				// rest of line is comment — replace with spaces
				for ; j < len(line); j++ {
					out.WriteByte(' ')
				}
				break
			}
			if line[j+1] == '*' {
				// block comment start — replace with spaces until */
				out.WriteByte(' ')
				out.WriteByte(' ')
				j++
				for j+1 < len(line) {
					if line[j] == '*' && line[j+1] == '/' {
						out.WriteByte(' ')
						out.WriteByte(' ')
						j++
						break
					}
					out.WriteByte(' ')
					j++
				}
				continue
			}
		}
		if inString {
			if stringChar == 0 {
				// block comment
				if ch == '*' && j+1 < len(line) && line[j+1] == '/' {
					out.WriteByte(' ')
					out.WriteByte(' ')
					j++
					inString = false
				} else {
					out.WriteByte(' ')
				}
				continue
			}
			if ch == '\\' {
				out.WriteByte(' ')
				// backtick strings (raw) don't process escape sequences
				if stringChar != '`' {
					skip = true
					continue
				}
			}
			if ch == stringChar {
				inString = false
			}
			out.WriteByte(' ')
			continue
		}
		if ch == '"' || ch == '\'' || ch == '`' {
			// Check for Python triple-quotes (''' or """)
			if (ch == '\'' || ch == '"') && j+2 < len(line) && line[j+1] == ch && line[j+2] == ch {
				// Triple quote — replace rest of line with spaces (may close on same line or span lines)
				for ; j < len(line); j++ {
					out.WriteByte(' ')
				}
				break
			}
			inString = true
			stringChar = ch
			out.WriteByte(' ')
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}

// countLeadingSpaces returns the number of leading space/tab characters in line.
func countLeadingSpaces(line string) int {
	n := 0
	for _, ch := range line {
		if ch == ' ' || ch == '\t' {
			n++
		} else {
			break
		}
	}
	return n
}

// codegraph-go: a Go MCP server with SQLite indexing, auto-sync, and code intelligence.
//
// Tools: search, search_fts, files, context, explore, callees, callers, trace, impact, node, status, affected.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dorokuma/codegraph-go/db"
	"github.com/dorokuma/codegraph-go/extraction"
	"github.com/dorokuma/codegraph-go/sync"
	"github.com/dorokuma/codegraph-go/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type server struct {
	workdir      string
	database     *db.DB
	orchestrator *extraction.Orchestrator
	// watcher is set from the background index goroutine after auto-sync starts.
	watcher atomic.Pointer[sync.Watcher]
}

// runInit implements `codegraph-go init <root>` for hosts that pre-warm the
// index directory (e.g. reasonix). It only ensures the DB layout exists and
// returns quickly — full indexing happens when the MCP server starts.
func runInit(root string) error {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	database, err := db.Open(abs)
	if err != nil {
		return err
	}
	if err := database.Close(); err != nil {
		return err
	}
	log.Printf("init ok workdir=%s db=%s", abs, filepath.Join(abs, ".codegraph", "codegraph.db"))
	return nil
}

func main() {
	// Never block forever writing logs into an unread stderr pipe.
	setupSafeLog()

	// Subcommand: init <root>  (must be handled before flag.Parse)
	if len(os.Args) >= 2 && os.Args[1] == "init" {
		root := "."
		if len(os.Args) >= 3 {
			root = os.Args[2]
		}
		if err := runInit(root); err != nil {
			log.Fatalf("init: %v", err)
		}
		return
	}

	var workdir string
	var noSync bool
	flag.StringVar(&workdir, "workdir", "", "workspace root (default: cwd)")
	flag.BoolVar(&noSync, "no-sync", false, "disable auto-sync file watcher")
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

	// Open database
	database, err := db.Open(workdir)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	// Create orchestrator
	orch := extraction.NewOrchestrator(database, workdir)

	s := &server{
		workdir:      workdir,
		database:     database,
		orchestrator: orch,
	}

	// Index + watcher in the background so MCP initialize is never blocked
	// by a multi-minute cold scan of a large workspace.
	go func() {
		log.Printf("indexing project in background...")
		files, nodes, err := orch.IndexAll()
		if err != nil {
			log.Printf("index warning: %v", err)
		}
		log.Printf("indexed %d files, %d nodes", files, nodes)

		if noSync {
			return
		}
		watcher, err := sync.NewWatcher(orch, workdir)
		if err != nil {
			log.Printf("watcher warning: %v", err)
			return
		}
		if err := watcher.Start(); err != nil {
			log.Printf("watcher start warning: %v", err)
			return
		}
		s.watcher.Store(watcher)
		log.Printf("auto-sync enabled")
	}()

	srv := mcp.NewServer(&mcp.Implementation{Name: "codegraph-go", Version: "0.4.0"}, nil)

	// 12 tools: search, search_fts, files, context, explore, callees, callers, trace, impact, node, status, affected.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search",
		Description: "ripgrep-style text/regex search across the workspace. Returns matching file paths with line numbers.",
	}, s.toolSearch)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_fts",
		Description: "Full-text search over indexed symbols using SQLite FTS5. Faster for symbol/name lookups than ripgrep.",
	}, s.toolSearchFTS)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "files",
		Description: "List files in the workspace matching a glob pattern (supports ** recursion). Uses ripgrep (respects .gitignore).",
	}, s.toolFiles)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "context",
		Description: "Read a window of lines around a given file:line position. Use after search to grab surrounding code.",
	}, s.toolContext)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "explore",
		Description: "List candidate entry points: top-level directories, README files, package manifests. Cheap first step on a new repo.",
	}, s.toolExplore)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "callees",
		Description: "List functions that <symbol> calls — extracts function names from the body of the matched definition.",
	}, s.toolCallees)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "callers",
		Description: "Find references (call sites) to `name` across the workspace.",
	}, s.toolCallers)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "trace",
		Description: "Grep for `name` across the workspace; depth=1 adds 5 lines of context per match.",
	}, s.toolTrace)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "impact",
		Description: "Reverse impact: which files reference `name`? Returns a sorted file list with match counts.",
	}, s.toolImpact)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "node",
		Description: "Get detailed information about a symbol: source code, callers, callees. Use after search to get full context.",
	}, s.toolNode)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "status",
		Description: "Check index health and statistics: node count, edge count, file count, pending sync files.",
	}, s.toolStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "affected",
		Description: "Find test files affected by changed source files. Traces import dependencies transitively.",
	}, s.toolAffected)

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server exited: %v", err)
	}

	// Clean up watcher on exit
	if w := s.watcher.Load(); w != nil {
		w.Stop()
	}
}

// ---------- tool implementations ----------

type searchArgs struct {
	Pattern    string `json:"pattern"      jsonschema:"regex or literal pattern (ripgrep syntax)"`
	Path       string `json:"path,omitempty" jsonschema:"optional subdirectory under workspace,optional"`
	Glob       string `json:"glob,omitempty" jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"global match cap (default 70; per-file also capped),optional"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"case-insensitive search,optional"`
}

type searchFTSArgs struct {
	Query string `json:"query" jsonschema:"full-text query (FTS5 syntax; plain words work)"`
	Max   int    `json:"max,omitempty" jsonschema:"result cap (default 50),optional"`
}

func (s *server) toolSearchFTS(ctx context.Context, _ *mcp.CallToolRequest, args searchFTSArgs) (*mcp.CallToolResult, any, error) {
	if args.Query == "" {
		return nil, nil, fmt.Errorf("query is required")
	}
	if args.Max == 0 {
		args.Max = 50
	}
	nodes, err := s.database.FullTextSearch(args.Query, args.Max)
	if err != nil {
		return nil, nil, fmt.Errorf("fts search: %w", err)
	}
	if len(nodes) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no matches"}},
		}, nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FTS matches for %q (%d):\n", args.Query, len(nodes))
	for _, n := range nodes {
		rel := n.File
		if r, err := filepath.Rel(s.workdir, n.File); err == nil {
			rel = r
		}
		fmt.Fprintf(&b, "%s:%d  %s (%s)\n", rel, n.Line, n.Name, n.Kind)
	}
	text := truncateOutput(b.String(), defaultOutputChars)
	text = s.addStalenessWarning(text)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

func (s *server) toolSearch(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
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
		args.MaxResults = defaultSearchGlobal
	}
	perFile := searchPerFileCap(args.MaxResults)
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		fmt.Sprintf("--max-count=%d", perFile),
	)
	if args.IgnoreCase {
		rg.Args = append(rg.Args, "-i")
	}
	if args.Glob != "" {
		rg.Args = append(rg.Args, "--glob", args.Glob)
	}
	rg.Args = append(rg.Args, args.Pattern, root)
	out, _ := rg.Output()
	if len(out) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no matches"}},
		}, nil, nil
	}
	text := limitLines(string(out), args.MaxResults)
	text = truncateOutput(text, defaultOutputChars)
	// Add staleness warning
	text = s.addStalenessWarning(text)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

type filesArgs struct {
	Pattern string `json:"pattern,omitempty" jsonschema:"glob pattern relative to workspace, e.g. \"src/**/*.go\",optional"`
	Max     int    `json:"max,omitempty"     jsonschema:"cap (default 100),optional"`
}

func (s *server) toolFiles(ctx context.Context, _ *mcp.CallToolRequest, args filesArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pattern := args.Pattern
	if pattern == "" {
		pattern = "**/*"
	}
	if args.Max == 0 {
		args.Max = defaultFilesMax
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
	rg := exec.CommandContext(ctx, "rg", "--files", "-g", pattern, root)
	out, err := rg.Output()
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no files matched"}},
		}, nil, nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > args.Max {
		lines = lines[:args.Max]
	}
	var b strings.Builder
	for _, l := range lines {
		rel, err := filepath.Rel(root, l)
		if err == nil {
			fmt.Fprintln(&b, rel)
		}
	}
	text := truncateOutput(b.String(), defaultOutputChars)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
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

type nameArgs struct {
	Name       string `json:"name"                 jsonschema:"symbol name to look for"`
	Path       string `json:"path,omitempty"        jsonschema:"optional subdirectory under workspace,optional"`
	Glob       string `json:"glob,omitempty"        jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"cap (default 40),optional"`
}

func (s *server) toolCallees(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSymbolMax
	}
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	quoted := regexp.QuoteMeta(args.Name)
	defPattern := fmt.Sprintf(`(func\s+(\([^)]*\)\s*)?|def |defn |function |async function |fn |class )%s\b`, quoted)
	rgDef := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		"--max-count=20",
		"-e", defPattern, root)
	defOut, err := rgDef.Output()
	if err != nil || len(bytes.TrimSpace(defOut)) == 0 {
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

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: truncateOutput(b.String(), defaultOutputChars)}},
	}, nil, nil
}

func (s *server) toolCallers(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSymbolMax
	}
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	// Fetch more than we return so filtering defs still leaves enough call sites.
	rgCap := args.MaxResults * 3
	if rgCap < args.MaxResults {
		rgCap = args.MaxResults
	}
	if rgCap > 200 {
		rgCap = 200
	}
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		fmt.Sprintf("--max-count=%d", rgCap),
		"-w", args.Name, root)
	if args.Glob != "" {
		rg.Args = append(rg.Args, "--glob", args.Glob)
	}
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no references found"}}}, nil, nil
	}
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
			continue
		}
		if defRe.MatchString(cleaned) {
			continue
		}
		filtered = append(filtered, line)
		if len(filtered) >= args.MaxResults {
			break
		}
	}
	if len(filtered) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no references found"}}}, nil, nil
	}
	result := strings.Join(filtered, "\n")
	if len(filtered) >= args.MaxResults {
		result += fmt.Sprintf("\n... (max %d; narrow path/glob or raise max_results)", args.MaxResults)
	}
	result = truncateOutput(result, defaultOutputChars)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

type traceArgs struct {
	Name  string `json:"name"`
	Depth int    `json:"depth,omitempty" jsonschema:"follow-up grep depth, 0 or 1 (default 0),optional"`
}

func (s *server) toolTrace(ctx context.Context, _ *mcp.CallToolRequest, args traceArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.Depth > 1 {
		args.Depth = 1
	}
	root := s.workdir
	rgArgs := []string{"--line-number", "--no-heading", "--color=never", fmt.Sprintf("--max-count=%d", defaultTraceMax)}
	if args.Depth == 1 {
		rgArgs = append(rgArgs, "-C", "5")
	}
	rgArgs = append(rgArgs, "-w", args.Name, root)
	rg := exec.CommandContext(ctx, "rg", rgArgs...)
	out, _ := rg.Output()
	result := string(out)
	if result == "" {
		result = "no matches found for " + args.Name
	} else {
		result = limitLines(result, defaultTraceMax)
		result = truncateOutput(result, defaultOutputChars)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

func (s *server) toolImpact(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSymbolMax
	}
	root, err := s.resolvePath(args.Path)
	if err != nil {
		return nil, nil, err
	}
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		"-c",
		"-w", args.Name, root)
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no files reference " + args.Name}}}, nil, nil
	}
	result := limitLines(string(out), args.MaxResults)
	result = truncateOutput(result, defaultOutputChars)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

// ---------- new tools: node, status ----------

type nodeArgs struct {
	Name string `json:"name" jsonschema:"symbol name to look for"`
	File string `json:"file,omitempty" jsonschema:"optional file path to narrow search,optional"`
	Line int    `json:"line,omitempty" jsonschema:"optional line number to find exact symbol,optional"`
}

func (s *server) toolNode(ctx context.Context, _ *mcp.CallToolRequest, args nodeArgs) (*mcp.CallToolResult, any, error) {
	result, err := tools.ToolNode(ctx, s.database, tools.NodeArgs{
		Name: args.Name,
		File: args.File,
		Line: args.Line,
	})
	if err != nil {
		return nil, nil, err
	}
	if result == nil || len(result.Content) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no symbols found"}},
		}, nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.Content[0].Text}},
	}, nil, nil
}

type statusArgs struct {
	Path string `json:"path,omitempty" jsonschema:"optional path to check specific file status,optional"`
}

func (s *server) toolStatus(ctx context.Context, _ *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, any, error) {
	var pendingFiles []string
	if w := s.watcher.Load(); w != nil {
		pendingFiles = w.PendingFiles()
	}

	result, err := tools.ToolStatus(ctx, s.database, s.workdir, tools.StatusArgs{
		Path: args.Path,
	}, pendingFiles)
	if err != nil {
		return nil, nil, err
	}
	if result == nil || len(result.Content) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "error getting status"}},
		}, nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.Content[0].Text}},
	}, nil, nil
}

// ---------- helpers ----------

// addStalenessWarning adds a warning about pending sync files.
func (s *server) addStalenessWarning(text string) string {
	if w := s.watcher.Load(); w != nil {
		pending := w.PendingFiles()
		if len(pending) > 0 {
			var warning strings.Builder
			warning.WriteString("\n\n⚠️ **Warning**: The following files have been modified but not yet synced to the index:\n")
			for _, f := range pending {
				warning.WriteString(fmt.Sprintf("- %s\n", f))
			}
			warning.WriteString("\nThe index may be stale for these files. Consider reading them directly for the latest content.")
			text += warning.String()
		}
	}
	return text
}

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
				for ; j < len(line); j++ {
					out.WriteByte(' ')
				}
				break
			}
			if line[j+1] == '*' {
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
			if (ch == '\'' || ch == '"') && j+2 < len(line) && line[j+1] == ch && line[j+2] == ch {
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

// ---------- affected tool ----------

type affectedArgs struct {
	Files  []string `json:"files"                jsonschema:"list of changed source files"`
	Stdin  bool     `json:"stdin,omitempty"      jsonschema:"read file list from stdin,optional"`
	Depth  int      `json:"depth,omitempty"      jsonschema:"max dependency traversal depth (default 5),optional"`
	Filter string   `json:"filter,omitempty"     jsonschema:"custom glob to identify test files,optional"`
}

func (s *server) toolAffected(ctx context.Context, _ *mcp.CallToolRequest, args affectedArgs) (*mcp.CallToolResult, any, error) {
	result, err := tools.ToolAffected(ctx, s.database, s.workdir, tools.AffectedArgs{
		Files:  args.Files,
		Stdin:  args.Stdin,
		Depth:  args.Depth,
		Filter: args.Filter,
	})
	if err != nil {
		return nil, nil, err
	}
	if result == nil || len(result.Content) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no affected test files found"}},
		}, nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.Content[0].Text}},
	}, nil, nil
}

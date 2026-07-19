// codegraph-go: a Go MCP server with SQLite indexing, auto-sync, and code intelligence.
//
// MCP tools (official 8 + affected): explore, node, search, callers, callees, impact, files, status, affected.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/dorokuma/codegraph-go/daemon"
	"github.com/dorokuma/codegraph-go/db"
	"github.com/dorokuma/codegraph-go/extraction"
	"github.com/dorokuma/codegraph-go/sync"
	"github.com/dorokuma/codegraph-go/tools"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type server struct {
	workdir      string
	database     *db.DB
	orchestrator *extraction.Orchestrator
	// watcher is set from the background index goroutine after auto-sync starts.
	watcher atomic.Pointer[sync.Watcher]

	// Cross-project cache: resolved .codegraph root → open DB (step 4 projectPath).
	projectMu    stdsync.Mutex
	projectCache map[string]*db.DB
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

	// Canonicalize so daemon socket/lock converge across symlink paths.
	if rp, err := filepath.EvalSymlinks(workdir); err == nil && rp != "" {
		workdir = rp
	}
	log.Printf("codegraph-go starting, workdir=%s", workdir)

	// Decision order (official #411):
	//  1. CODEGRAPH_DAEMON_INTERNAL=1 → we ARE the detached daemon
	//  2. CODEGRAPH_NO_DAEMON=1 → direct embedded mode
	//  3. else try proxy to shared daemon (spawn if needed); fallback direct
	if daemon.Internal() {
		runDaemonProcess(workdir, noSync)
		return
	}
	if daemon.OptOut() {
		log.Printf("mode=direct (CODEGRAPH_NO_DAEMON)")
		runDirect(workdir, noSync)
		return
	}

	// Need a place for lock/socket. Prefer nearest existing .codegraph/;
	// otherwise open (creates) under workdir so subsequent sessions can share.
	root := db.FindNearestCodeGraphRoot(workdir)
	if root == "" {
		root = workdir
		// Ensure .codegraph exists so a daemon can be spawned.
		if database, err := db.Open(root); err == nil {
			_ = database.Close()
		}
	}
	if rp, err := filepath.EvalSymlinks(root); err == nil && rp != "" {
		root = rp
	}

	// Probe → spawn → dial shared daemon; on failure fall back to direct.
	conn, br, hello, ok := daemon.EnsureAndDial(root, 6*time.Second, 25*time.Millisecond)
	if ok {
		log.Printf("mode=proxy → daemon pid %d socket %s", hello.PID, hello.SocketPath)
		_ = daemon.RunProxy(conn, br, hello)
		return
	}
	log.Printf("mode=direct (daemon unavailable)")
	runDirect(workdir, noSync)
}

// runDirect is the pre-daemon single-process MCP server (stdio).
func runDirect(workdir string, noSync bool) {
	s, cleanup := openServerState(workdir, noSync)
	defer cleanup()

	srv := newMCPServer(s)
	stopWD := daemon.StartPPIDWatchdog(daemon.PPIDPollInterval(), func() {
		os.Exit(0)
	})
	defer stopWD()

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

// runDaemonProcess is CODEGRAPH_DAEMON_INTERNAL: single writer, N socket clients.
func runDaemonProcess(workdir string, noSync bool) {
	var (
		stateOnce stdsync.Once
		s         *server
		cleanup   func()
		mcpSrv    *mcp.Server
	)
	ensure := func() {
		stateOnce.Do(func() {
			s, cleanup = openServerState(workdir, noSync)
			mcpSrv = newMCPServer(s)
		})
	}
	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		ensure()
		// Each connection is one MCP session sharing tools/DB/watcher.
		// IOTransport closes Reader and Writer separately — once-wrap so the
		// underlying conn is closed exactly once.
		oc := &onceRWC{ReadWriteCloser: rwc}
		ss, err := mcpSrv.Connect(ctx, &mcp.IOTransport{
			Reader: oc,
			Writer: oc,
		}, nil)
		if err != nil {
			return err
		}
		return ss.Wait()
	}
	onReady := func() error {
		ensure()
		return nil
	}
	if err := daemon.RunAsDaemon(workdir, handler, onReady); err != nil {
		log.Fatalf("daemon: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
}

// onceRWC closes the underlying ReadWriteCloser at most once.
type onceRWC struct {
	io.ReadWriteCloser
	once stdsync.Once
	err  error
}

func (o *onceRWC) Close() error {
	o.once.Do(func() { o.err = o.ReadWriteCloser.Close() })
	return o.err
}

// openServerState opens DB + orchestrator and kicks background index/watcher.
func openServerState(workdir string, noSync bool) (*server, func()) {
	database, err := db.Open(workdir)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}

	// Start WAL checkpoint background loop (every 5 minutes).
	walCP := db.NewWALCheckpoint(database, 5*time.Minute)
	walCP.Start()

	orch := extraction.NewOrchestrator(database, workdir)
	s := &server{
		workdir:      workdir,
		database:     database,
		orchestrator: orch,
	}
	go backgroundIndexAndWatch(s, noSync)
	cleanup := func() {
		walCP.Stop()
		if w := s.watcher.Load(); w != nil {
			w.Stop()
		}
		s.closeProjectCache()
		_ = database.Close()
	}
	return s, cleanup
}

func backgroundIndexAndWatch(s *server, noSync bool) {
	database := s.database
	orch := s.orchestrator
	workdir := s.workdir

	rebuild, oldVer, err := database.NeedsRebuild()
	if err != nil {
		log.Printf("logic version check: %v", err)
	}
	var files, nodes int
	if rebuild {
		log.Printf("index logic %s → %s: full rebuild...", oldVer, db.LogicVersion())
		files, nodes, err = orch.RebuildAll()
	} else {
		log.Printf("indexing project in background...")
		files, nodes, err = orch.IndexAll()
		if err == nil {
			_ = database.SetLogicVersion()
		}
	}
	if err != nil {
		log.Printf("index warning: %v", err)
	}
	log.Printf("indexed %d files, %d nodes (logic=%s)", files, nodes, db.LogicVersion())

	// Optional git-status assist: catch edits missed while nothing was watching.
	if dirty := sync.GitDirtySourceFiles(workdir); len(dirty) > 0 {
		c, n, gerr := orch.IndexChanges(dirty)
		if gerr != nil {
			log.Printf("git-assist sync: %v", gerr)
		} else if c > 0 {
			log.Printf("git-assist sync: %d files, %d nodes", c, n)
		}
	}

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
}

// newMCPServer registers the official 8 + affected tools.
func newMCPServer(s *server) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "codegraph-go", Version: daemon.PackageVersion}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "explore",
		Description: "PRIMARY TOOL — call FIRST for almost any question or before an edit: how does X work, architecture, a bug, where/what is X, or the symbols you are about to change. " +
			"Returns verbatim source of relevant symbols PLUS the call path among them (Flow). Query can be a natural-language question OR a bag of symbol/file names. " +
			"Treat returned source as already Read — do NOT re-open those files. Usually the ONLY call you need.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"query":       {Type: "string", Description: "symbol or free-text; empty = overview"},
				"path":        {Type: "string", Description: "optional project subdirectory (home mode)"},
				"max":         {Type: "integer", Description: "cap on entries (default 30, hard max 60)"},
				"skipCode":    {Type: "boolean", Description: "omit source code from results (default true). Set false to include implementation bodies."},
				"projectPath": {Type: "string", Description: "absolute path to the project to query"},
			},
		},
	}, s.toolExplore)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "node",
		Description: "SECONDARY. Two modes. (1) READ A FILE — pass `file` alone (no name): on-disk source with line numbers like Read (`<n>\\t<line>`), plus which files depend on it; offset/limit like Read; symbolsOnly for a cheap map. " +
			"(2) ONE SYMBOL — pass `name`: location, body (includeCode default true), caller/callee trail. Overloaded names return EVERY matching body in one call; pass file/line to pin one. Prefer explore for multi-symbol flows.",
	}, s.toolNode)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search",
		Description: "Find symbols or text. Simple identifier → indexed FTS first (replaces a separate search_fts tool); regex/path/glob use ripgrep. Prefer explore when you already know related names.",
	}, s.toolSearch)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "callers",
		Description: "Who calls this symbol? Graph first; rg fallback if no edges. Pass file to pin an overloaded name. For full flow use explore.",
	}, s.toolCallers)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "callees",
		Description: "What does this symbol call? Graph first; body-parse fallback if no edges. Pass file to pin an overloaded name. For full flow use explore.",
	}, s.toolCallees)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "impact",
		Description: "Blast radius if you change this symbol. Graph BFS first; rg counts as fallback. Pass file to pin an overloaded name.",
	}, s.toolImpact)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "files",
		Description: "List files matching a glob (supports **). Uses ripgrep; respects .gitignore.",
	}, s.toolFiles)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "status",
		Description: "Index health: node/edge/file counts and pending sync files. Skip unless debugging.",
	}, s.toolStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "affected",
		Description: "SECONDARY extension (not on official MCP). Which tests are affected by changed source files? Pass files= after edits; not the main navigation path — prefer explore/node first.",
	}, s.toolAffected)

	return srv
}

// ---------- tool implementations ----------

type searchArgs struct {
	Pattern     string `json:"pattern"      jsonschema:"regex or literal pattern (ripgrep syntax)"`
	Path        string `json:"path,omitempty" jsonschema:"optional subdirectory under workspace,optional"`
	Glob        string `json:"glob,omitempty" jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults  int    `json:"max_results,omitempty" jsonschema:"global match cap (default 70; per-file also capped),optional"`
	IgnoreCase  bool   `json:"ignore_case,omitempty" jsonschema:"case-insensitive search,optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

func (s *server) toolSearch(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Pattern == "" {
		return nil, nil, fmt.Errorf("pattern is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSearchGlobal
	}
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Pattern); p != "" {
			args.ProjectPath = p
		}
	}
	projRoot, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}

	// Official CodeGraph search is symbol-first. For a plain identifier with no
	// path/glob/regex metacharacters, hit FTS before spawning rg.
	if args.Path == "" && args.Glob == "" && !args.IgnoreCase && isSimpleIdent(args.Pattern) {
		nodes, err := database.FullTextSearch(args.Pattern, args.MaxResults)
		if err == nil && len(nodes) > 0 {
			var b strings.Builder
			for _, n := range nodes {
				fmt.Fprintf(&b, "%s:%d\n", db.RelPath(projRoot, n.File), n.Line)
			}
			text := truncateOutput(b.String(), defaultOutputChars)
			text = s.addStalenessWarning(text)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
			}, nil, nil
		}
	}

	root, err := s.resolvePathIn(projRoot, args.Path)
	if err != nil {
		return nil, nil, err
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
	rg.Args = append(rg.Args, "--", args.Pattern, root)
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		// rg exits 1 on no matches; other errors should surface.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "no matches"}},
			}, nil, nil
		}
		return nil, nil, fmt.Errorf("rg search: %w", err)
	}
	if len(out) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no matches"}},
		}, nil, nil
	}
	text := limitLines(string(out), args.MaxResults)
	text = truncateOutput(text, defaultOutputChars)
	text = s.addStalenessWarning(text)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

// isSimpleIdent reports whether s looks like a bare symbol name (no regex).
func isSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '$':
			continue
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type filesArgs struct {
	Pattern     string `json:"pattern,omitempty" jsonschema:"glob pattern relative to workspace, e.g. \"src/**/*.go\",optional"`
	Max         int    `json:"max,omitempty"     jsonschema:"cap (default 100),optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
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
	root, _, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
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

type exploreArgs struct {
	Query       string `json:"query,omitempty" jsonschema:"symbol or free-text; empty = project overview,optional"`
	Path        string `json:"path,omitempty" jsonschema:"optional project subdirectory (home mode),optional"`
	Max         int    `json:"max,omitempty" jsonschema:"cap on files shown (0 = size-tier default; max 100),optional"`
	SkipCode    bool   `json:"skipCode,omitempty" jsonschema:"omit source bodies; show location + trail only,optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

func (s *server) toolExplore(ctx context.Context, _ *mcp.CallToolRequest, args exploreArgs) (*mcp.CallToolResult, any, error) {
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Query); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	text, err := tools.ToolExplore(ctx, database, root, tools.ExploreArgs{
		Query:    args.Query,
		Path:     args.Path,
		Max:      args.Max,
		SkipCode: args.SkipCode,
	})
	if err != nil {
		return nil, nil, err
	}
	text = truncateOutput(text, defaultOutputChars)
	text = s.addStalenessWarning(text)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

type nameArgs struct {
	Name        string `json:"name"                 jsonschema:"symbol name to look for"`
	File        string `json:"file,omitempty"        jsonschema:"narrow to the definition in this file (path or basename) when several same-named symbols exist,optional"`
	Path        string `json:"path,omitempty"        jsonschema:"optional subdirectory under workspace,optional"`
	Glob        string `json:"glob,omitempty"        jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults  int    `json:"max_results,omitempty" jsonschema:"cap (default 40),optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
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
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}

	// Graph-first (official CodeGraph path).
	if text, ok, err := tools.ToolCalleesGraph(database, root, tools.GraphQueryArgs{
		Name: args.Name, Path: args.Path, File: args.File, Glob: args.Glob, MaxResults: args.MaxResults,
	}); err != nil {
		return nil, nil, err
	} else if ok {
		text = truncateOutput(text, defaultOutputChars)
		text = s.addStalenessWarning(text)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}

	// Fallback: body-parse via rg (legacy path in callees_fallback.go).
	// Body fallback still uses session default workdir paths via s.workdir;
	// swap temporarily is awkward — pass project via resolvePathIn in fallback.
	return s.toolCalleesBodyFallback(ctx, args)
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
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}

	// Graph-first (official CodeGraph path).
	if text, ok, err := tools.ToolCallersGraph(database, root, tools.GraphQueryArgs{
		Name: args.Name, Path: args.Path, File: args.File, Glob: args.Glob, MaxResults: args.MaxResults,
	}); err != nil {
		return nil, nil, err
	} else if ok {
		text = truncateOutput(text, defaultOutputChars)
		text = s.addStalenessWarning(text)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}

	// Fallback: ripgrep references (labeled so the agent knows).
	searchRoot, err := s.resolvePathIn(root, args.Path)
	if err != nil {
		return nil, nil, err
	}
	rgCap := args.MaxResults * 3
	if rgCap > 200 {
		rgCap = 200
	}
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		fmt.Sprintf("--max-count=%d", rgCap),
		"-w", args.Name, searchRoot)
	if args.Glob != "" {
		rg.Args = append(rg.Args, "--glob", args.Glob)
	}
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no references found (index empty for this symbol; rg fallback also empty)"}}}, nil, nil
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
	result := "# Callers of " + args.Name + " (rg fallback — index had no call edges)\n" + strings.Join(filtered, "\n")
	if len(filtered) >= args.MaxResults {
		result += fmt.Sprintf("\n... (max %d; narrow path/glob or raise max_results)", args.MaxResults)
	}
	result = truncateOutput(result, defaultOutputChars)
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
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name); p != "" {
			args.ProjectPath = p
		}
	}
	projRoot, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}

	// Graph BFS first (official getImpactRadius).
	if text, ok, err := tools.ToolImpactGraph(database, projRoot, tools.GraphQueryArgs{
		Name: args.Name, Path: args.Path, File: args.File, Glob: args.Glob, MaxResults: args.MaxResults, Depth: 2,
	}); err != nil {
		return nil, nil, err
	} else if ok {
		text = truncateOutput(text, defaultOutputChars)
		text = s.addStalenessWarning(text)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}

	root, err := s.resolvePathIn(projRoot, args.Path)
	if err != nil {
		return nil, nil, err
	}
	rg := exec.CommandContext(ctx, "rg",
		"--line-number", "--no-heading", "--color=never",
		"-c", "-w", args.Name, root)
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no files reference " + args.Name}}}, nil, nil
	}
	result := "# Impact of " + args.Name + " (rg fallback)\n" + limitLines(string(out), args.MaxResults)
	result = truncateOutput(result, defaultOutputChars)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

// ---------- node, status ----------

type nodeArgs struct {
	Name         string `json:"name,omitempty" jsonschema:"symbol name (symbol mode). Omit and pass file alone to read a whole file like Read.,optional"`
	File         string `json:"file,omitempty" jsonschema:"file path or basename. Alone = file-read mode; with name = disambiguate overload.,optional"`
	Line         int    `json:"line,omitempty" jsonschema:"symbol mode: pin definition at/around this line,optional"`
	IncludeCode  *bool  `json:"includeCode,omitempty" jsonschema:"symbol mode: include body (default true),optional"`
	SymbolsOnly  bool   `json:"symbolsOnly,omitempty" jsonschema:"file mode: symbol map + dependents only,optional"`
	Offset       int    `json:"offset,omitempty" jsonschema:"file mode: 1-based start line (like Read),optional"`
	Limit        int    `json:"limit,omitempty" jsonschema:"file mode: max lines (default whole file, cap 2000),optional"`
	ProjectPath  string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

func (s *server) toolNode(ctx context.Context, _ *mcp.CallToolRequest, args nodeArgs) (*mcp.CallToolResult, any, error) {
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	// Keep original file hint for basename matching; only absolutize when it resolves under root.
	file := args.File
	if file != "" && strings.TrimSpace(args.Name) != "" {
		if resolved, err := s.resolvePathIn(root, file); err == nil {
			// Prefer absolute when it exists on disk or in index; basename still works via ToolNodeIn.
			if _, statErr := os.Stat(resolved); statErr == nil {
				file = resolved
			}
		}
	}
	result, err := tools.ToolNodeIn(ctx, database, root, tools.NodeArgs{
		Name:        args.Name,
		File:        file,
		Line:        args.Line,
		IncludeCode: args.IncludeCode,
		SymbolsOnly: args.SymbolsOnly,
		Offset:      args.Offset,
		Limit:       args.Limit,
	})
	if err != nil {
		return nil, nil, err
	}
	if result == nil || len(result.Content) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no symbols found"}},
		}, nil, nil
	}
	outCap := defaultOutputChars
	if result.FileMode {
		outCap = 38_000 // file-read parity with official; don't chop like a symbol dump
	}
	text := truncateOutput(result.Content[0].Text, outCap)
	text = s.addStalenessWarning(text)

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

type statusArgs struct {
	Path        string `json:"path,omitempty" jsonschema:"optional path to check specific file status,optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

func (s *server) toolStatus(ctx context.Context, _ *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, any, error) {
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	var pendingFiles []string
	// Pending files only apply to the default session watcher.
	if root == s.workdir {
		if w := s.watcher.Load(); w != nil {
			pendingFiles = w.PendingFiles()
		}
	}

	result, err := tools.ToolStatus(ctx, database, root, tools.StatusArgs{
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
	return s.resolvePathIn(s.workdir, p)
}

// resolvePathIn joins p under root and rejects escapes outside root.
func (s *server) resolvePathIn(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	var target string
	if filepath.IsAbs(p) {
		target = filepath.Clean(p)
	} else {
		target = filepath.Clean(filepath.Join(root, p))
	}
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside workspace %q", p, root)
	}
	return target, nil
}

// serverInstructions is sent in MCP initialize so agents learn the playbook once.
const serverInstructions = `# Codegraph-go — code intelligence over an indexed knowledge graph

Codegraph-go is a SQLite knowledge graph of symbols, edges, and files in the
workspace. Reach for it BEFORE and while editing — one call returns verbatim
source PLUS who calls it and what it affects. More accurate context, fewer
tokens and round-trips than a Read/Grep loop.

## Primary: explore

- Almost any question ("how does X work", architecture, a bug, survey) →
  **explore** with a natural-language question or bag of symbol/file names.
  ONE call returns source grouped by file + Flow path. Treat that source as
  already Read — do NOT re-open those files.
- Flow from X to Y → explore naming both ends (e.g. "mutateElement renderScene").

## Secondary: node / search / graph

- Read one file like Read → **node** with file only (line-numbered source +
  dependents). offset/limit work like Read; symbolsOnly for a cheap map.
- One named symbol (body + caller/callee trail) → **node** with name.
  Overloads return every body in one call; pass file/line to pin one.
- Find a name → **search** (simple identifiers hit the index FTS first).
- Who calls / what it calls / blast radius → **callers** / **callees** / **impact**
  (pass file when the name is overloaded). Prefer explore for multi-hop flows.
- Layout → **files**. Index health → **status**. After edits, which tests →
  **affected** (extension; not the main navigation path).

## Anti-patterns

- Don't re-verify codegraph with grep — the index is AST-based.
- Don't Read/Grep first for indexed code — explore/node already return source.
- Don't reconstruct a flow by hand — name the endpoints in one explore.
- If a tool says a project isn't indexed, stop calling codegraph for THAT
  project this session and use built-in tools there; other projectPath targets
  still work. Indexing is the user's decision (codegraph-go init).
- Index lags writes by ~1–2s via the file watcher.
`

// recoverableProjectErr turns "not indexed" into a success-shaped guidance
// result (no isError) so agents don't abandon codegraph for the whole session.
func recoverableProjectErr(err error) (*mcp.CallToolResult, any, error) {
	if err == nil {
		return nil, nil, nil
	}
	msg := err.Error()
	if strings.Contains(msg, "no .codegraph index") || strings.Contains(msg, "isn't indexed") {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: msg + "\nUse built-in Read/Grep for that path this session, or pass projectPath to an indexed project."}},
		}, nil, nil
	}
	return nil, nil, err
}

// resolveProject picks the DB + root for a tool call.
// Empty projectPath → session default. Non-empty → walk up to nearest .codegraph/.
// isWordIn reports whether word appears as a standalone word in text.
// Word boundaries are: start/end of string, space, slash, dot, dash, underscore.
func isWordIn(word, text string) bool {
	idx := strings.Index(text, word)
	if idx < 0 {
		return false
	}
	end := idx + len(word)
	leftOK := idx == 0 || isWordSep(text[idx-1])
	rightOK := end == len(text) || isWordSep(text[end])
	return leftOK && rightOK
}

func isWordSep(b byte) bool {
	switch b {
	case ' ', '/', '.', '-', '_', ',', ':', '\t', '\n', '(', ')', '[', ']', '{', '}':
		return true
	}
	return false
}

// detectProject tries to find which project the user is asking about
// by matching query/args against project directory names under workdir.
// Returns the project dir name (relative to workdir) or empty string.
func (s *server) detectProject(queries ...string) string {
	if !extraction.IsBroadWorkdir(s.workdir) {
		return ""
	}
	entries, err := os.ReadDir(s.workdir)
	if err != nil {
		return ""
	}
	for _, q := range queries {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			projectDir := filepath.Join(s.workdir, e.Name())
			if !extraction.HasProjectMarker(projectDir) {
				continue
			}
			if strings.ToLower(e.Name()) == q {
				return e.Name()
			}
		}
	}
	// Fuzzy: check if any project name appears as a word in the query
	for _, q := range queries {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			projectDir := filepath.Join(s.workdir, e.Name())
			if !extraction.HasProjectMarker(projectDir) {
				continue
			}
			name := strings.ToLower(e.Name())
			if isWordIn(name, q) {
				return e.Name()
			}
		}
	}
	return ""
}

func (s *server) resolveProject(projectPath string) (root string, database *db.DB, err error) {
	if strings.TrimSpace(projectPath) == "" {
		return s.workdir, s.database, nil
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return "", nil, fmt.Errorf("bad projectPath %q: %w", projectPath, err)
	}
	resolved := db.FindNearestCodeGraphRoot(abs)
	if resolved == "" {
		return "", nil, fmt.Errorf(
			"no .codegraph index found walking up from %s; pass a path inside an indexed project, or omit projectPath to use the session default",
			abs,
		)
	}
	if resolved == s.workdir {
		return s.workdir, s.database, nil
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if s.projectCache == nil {
		s.projectCache = map[string]*db.DB{}
	}
	if cached, ok := s.projectCache[resolved]; ok {
		return resolved, cached, nil
	}
	opened, err := db.Open(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("open index at %s: %w", resolved, err)
	}
	s.projectCache[resolved] = opened
	return resolved, opened, nil
}

func (s *server) closeProjectCache() {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	for root, d := range s.projectCache {
		_ = d.Close()
		delete(s.projectCache, root)
	}
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
	Files       []string `json:"files"                jsonschema:"list of changed source files"`
	Stdin       bool     `json:"stdin,omitempty"      jsonschema:"read file list from stdin,optional"`
	Depth       int      `json:"depth,omitempty"      jsonschema:"max dependency traversal depth (default 5),optional"`
	Filter      string   `json:"filter,omitempty"     jsonschema:"custom glob to identify test files,optional"`
	ProjectPath string   `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

func (s *server) toolAffected(ctx context.Context, _ *mcp.CallToolRequest, args affectedArgs) (*mcp.CallToolResult, any, error) {
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	result, err := tools.ToolAffected(ctx, database, root, tools.AffectedArgs{
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

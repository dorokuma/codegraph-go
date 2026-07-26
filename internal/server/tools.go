package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/dorokuma/codegraph-go/internal/daemon"
	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
	"github.com/dorokuma/codegraph-go/internal/tools"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------- args types ----------

type searchArgs struct {
	Pattern     string `json:"pattern"      jsonschema:"regex or literal pattern (ripgrep syntax)"`
	Path        string `json:"path,omitempty" jsonschema:"optional subdirectory under workspace,optional"`
	Glob        string `json:"glob,omitempty" jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults  int    `json:"max_results,omitempty" jsonschema:"global match cap (default 70; per-file also capped),optional"`
	IgnoreCase  bool   `json:"ignore_case,omitempty" jsonschema:"case-insensitive search,optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type filesArgs struct {
	Pattern     string `json:"pattern,omitempty" jsonschema:"glob pattern relative to workspace, e.g. \"src/**/*.go\",optional"`
	Path        string `json:"path,omitempty" jsonschema:"optional subdirectory under workspace,optional"`
	Max         int    `json:"max,omitempty"     jsonschema:"cap (default 100),optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type exploreArgs struct {
	Query       string `json:"query,omitempty" jsonschema:"symbol or free-text; empty = project overview,optional"`
	Path        string `json:"path,omitempty" jsonschema:"optional project subdirectory (home mode),optional"`
	Max         int    `json:"max,omitempty" jsonschema:"cap on files shown (0 = size-tier default; max 100),optional"`
	SkipCode    *bool  `json:"skipCode,omitempty" jsonschema:"omit source bodies; show location + trail only,optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type nameArgs struct {
	Name        string `json:"name"                 jsonschema:"symbol name to look for"`
	File        string `json:"file,omitempty"        jsonschema:"narrow to the definition in this file (path or basename) when several same-named symbols exist,optional"`
	Path        string `json:"path,omitempty"        jsonschema:"optional subdirectory under workspace,optional"`
	Glob        string `json:"glob,omitempty"        jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults  int    `json:"max_results,omitempty" jsonschema:"cap (default 40),optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type nodeArgs struct {
	Name        string `json:"name,omitempty" jsonschema:"symbol name (symbol mode). Omit and pass file alone to read a whole file like Read.,optional"`
	File        string `json:"file,omitempty" jsonschema:"file path or basename. Alone = file-read mode; with name = disambiguate overload.,optional"`
	Line        int    `json:"line,omitempty" jsonschema:"symbol mode: pin definition at/around this line,optional"`
	IncludeCode *bool  `json:"includeCode,omitempty" jsonschema:"symbol mode: include body (default false; set true to include source),optional"`
	SymbolsOnly bool   `json:"symbolsOnly,omitempty" jsonschema:"file mode: symbol map + dependents only,optional"`
	Offset      int    `json:"offset,omitempty" jsonschema:"file mode: 1-based start line (like Read),optional"`
	Limit       int    `json:"limit,omitempty" jsonschema:"file mode: max lines (default whole file, cap 2000),optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type statusArgs struct {
	Path        string `json:"path,omitempty" jsonschema:"optional path to check specific file status,optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type affectedArgs struct {
	Files       []string `json:"files"                jsonschema:"list of changed source files"`
	Stdin       bool     `json:"stdin,omitempty"      jsonschema:"unsupported over MCP (stdio is the protocol); pass files instead,optional"`
	Depth       int      `json:"depth,omitempty"      jsonschema:"max dependency traversal depth (default 5),optional"`
	Filter      string   `json:"filter,omitempty"     jsonschema:"custom glob to identify test files,optional"`
	ProjectPath string   `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type communitiesArgs struct {
	Max         int    `json:"max,omitempty"      jsonschema:"max communities to report (default 20),optional"`
	MinSize     int    `json:"minSize,omitempty"  jsonschema:"minimum community size (nodes) to include in output (default 3),optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type storeFactArgs struct {
	TargetFile    string `json:"targetFile"             jsonschema:"file path the fact pertains to (required)"`
	TargetSymbol  string `json:"targetSymbol,omitempty"  jsonschema:"symbol name within the file,optional"`
	TargetLine    int    `json:"targetLine,omitempty"    jsonschema:"line number within the file,optional"`
	Content       string `json:"content"                jsonschema:"fact content (required)"`
	Author        string `json:"author,omitempty"        jsonschema:"who wrote this fact,optional"`
	Supersedes    int64  `json:"supersedes,omitempty"    jsonschema:"fact id this replaces (creates a supersede chain),optional"`
	ProjectPath   string `json:"projectPath,omitempty"  jsonschema:"absolute path to the project to write to (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type searchFactsArgs struct {
	Query        string `json:"query,omitempty"       jsonschema:"search term in fact content (case-insensitive substring match),optional"`
	TargetFile   string `json:"targetFile,omitempty"   jsonschema:"filter by target file path,optional"`
	TargetSymbol string `json:"targetSymbol,omitempty" jsonschema:"filter by target symbol,optional"`
	Status       string `json:"status,omitempty"       jsonschema:"filter by status (default 'active'; pass 'all' for all statuses),optional"`
	Max          int    `json:"max,omitempty"           jsonschema:"max results (default 20),optional"`
	ProjectPath  string `json:"projectPath,omitempty"  jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

// ---------- newMCPServer ----------

// NewMCPServer registers the official 8 + affected tools and returns the MCP server.
func NewMCPServer(s *Server) *mcp.Server {
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
			"(2) ONE SYMBOL — pass `name`: location, body (includeCode default false; set true to include source), caller/callee trail. Overloaded names return EVERY matching body in one call; pass file/line to pin one. Prefer explore for multi-symbol flows.",
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
		Name:        "store_fact",
		Description: "Store a fact (finding / decision / note) attached to a code symbol. " +
			"If content hash already exists, returns the existing record with duplicate=true. " +
			"If supersedes is provided, marks the superseded fact as 'superseded' and links it. " +
			"Returns the stored fact plus any other active facts for the same target, letting the " +
			"calling LLM detect contradiction/update scenarios.",
	}, s.toolStoreFact)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_facts",
		Description: "Search stored facts by content, file, symbol, or status. Facts survive index rebuilds.",
	}, s.toolSearchFacts)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "affected",
		Description: "SECONDARY extension (not on official MCP). Which tests are affected by changed source files? Pass files= after edits; not the main navigation path — prefer explore/node first.",
	}, s.toolAffected)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "communities",
		Description: "Run community detection on the project call graph to reveal module/component boundaries. Global architecture questions: how is this project organized, what are the main modules, what's the architecture backbone. Uses Louvain modularity (resolution=1.0) on a weighted undirected graph projected from directed edges (contains edges excluded). Communities sorted by size, each showing top symbols by weighted degree, top files, and kind distribution.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"max":         {Type: "integer", Description: "max communities to report (default 20)"},
				"minSize":     {Type: "integer", Description: "minimum community size to include in output (default 3)"},
				"projectPath": {Type: "string", Description: "absolute path to the project to query"},
			},
		},
	}, s.toolCommunities)

	return srv
}

// ---------- tool implementations ----------

func (s *Server) toolSearch(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Pattern == "" {
		return nil, nil, fmt.Errorf("pattern is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSearchGlobal
	}
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Pattern, args.Path); p != "" {
			args.ProjectPath = p
		}
	}
	projRoot, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(projRoot)

	// Official CodeGraph search is symbol-first. For a plain identifier with no
	// path/glob/regex metacharacters, hit FTS before spawning rg.
	if args.Path == "" && args.Glob == "" && !args.IgnoreCase && isSimpleIdent(args.Pattern) {
		nodes, err := database.FullTextSearchContext(ctx, args.Pattern, args.MaxResults)
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
	// When the user specifies a path they intend to search that directory
	// regardless of .gitignore rules. Otherwise a seemingly thorough search
	// silently skips ignored files.
	if args.Path != "" {
		rg.Args = append(rg.Args, "--no-ignore")
	}
	rg.Args = append(rg.Args, "--", args.Pattern, root)
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		// rg exits 1 on no matches; other errors should surface.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			msg := "no matches"
			if args.Path != "" {
				indexed, cerr := countIndexedUnder(ctx, database, projRoot, root)
				if cerr == nil && indexed == 0 {
					msg = fmt.Sprintf("no matches; path %q may not be indexed (0 indexed files under %s)", args.Path, root)
				}
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: msg}},
			}, nil, nil
		}
		return nil, nil, fmt.Errorf("rg search: %w", err)
	}
	if len(out) == 0 {
		// When a path subdirectory is specified, the user may be searching
		// an unindexed area. Check whether any files are indexed under root
		// and include a hint so the agent knows to use built-in tools.
		msg := "no matches"
		if args.Path != "" {
			indexed, cerr := countIndexedUnder(ctx, database, projRoot, root)
			if cerr == nil && indexed == 0 {
				msg = fmt.Sprintf("no matches; path %q may not be indexed (0 indexed files under %s)", args.Path, root)
			}
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		}, nil, nil
	}
	text := relativizeRgOutput(string(out), projRoot)
	text = limitLines(text, args.MaxResults)
	text = truncateOutput(text, defaultOutputChars)
	text = s.addStalenessWarning(text)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

func (s *Server) toolFiles(ctx context.Context, _ *mcp.CallToolRequest, args filesArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pattern := args.Pattern
	if pattern == "" {
		pattern = "**/*"
	}
	if args.Max == 0 {
		args.Max = defaultFilesMax
	}
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Path); p != "" {
			args.ProjectPath = p
		}
	}
	root, _, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)

	// Home/broad mode: rg would search the entire home directory (go/pkg/mod,
	// other projects, etc.). Use the DB file list instead — it only contains
	// files that passed the indexer's home-mode filtering.
	anyBroad := false
	for _, wd := range s.Workdirs {
		if extraction.IsBroadWorkdir(wd) {
			anyBroad = true
			break
		}
	}
	if anyBroad && args.ProjectPath == "" {
		// Narrow pattern if a path subdirectory is specified.
		effectivePattern := pattern
		if args.Path != "" {
			effectivePattern = filepath.Join(args.Path, pattern)
		}
		text, ferr := s.listFilesByGlob(effectivePattern, args.Max)
		if ferr != nil {
			return nil, nil, ferr
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	}

	// Narrow search root if a path subdirectory is specified.
	if args.Path != "" {
		root, err = s.resolvePathIn(root, args.Path)
		if err != nil {
			return nil, nil, err
		}
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
		// rg exits 1 when nothing matched; other failures must surface.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "no files matched"}},
			}, nil, nil
		}
		if len(out) == 0 {
			return nil, nil, fmt.Errorf("rg files: %w", err)
		}
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

// listFilesByGlob returns indexed files matching pattern, using the DB.
// Supports ** for recursive directory matching (e.g. **/*.go, src/**/*.ts).
func (s *Server) listFilesByGlob(pattern string, max int) (string, error) {
	allFiles, err := s.Database.ListFiles()
	if err != nil {
		return "", fmt.Errorf("list indexed files: %w", err)
	}
	root := s.Workdir
	var matched []string
	for _, p := range allFiles {
		rel := db.RelPath(root, p)
		if rel == "" || strings.HasPrefix(rel, "..") {
			continue
		}
		if !globMatch(pattern, rel) {
			continue
		}
		matched = append(matched, rel)
		if len(matched) >= max {
			break
		}
	}
	if len(matched) == 0 {
		return "no files matched", nil
	}
	return strings.Join(matched, "\n") + "\n", nil
}

// globMatch supports ** for recursive directory matching in addition to
// the standard * and ? single-segment patterns.
func globMatch(pattern, relPath string) bool {
	// Normalize to forward slashes for consistent matching.
	p := filepath.ToSlash(pattern)
	s := filepath.ToSlash(relPath)
	matched, _ := doublestar.Match(p, s)
	return matched
}

func (s *Server) toolExplore(ctx context.Context, _ *mcp.CallToolRequest, args exploreArgs) (*mcp.CallToolResult, any, error) {
	// Default skipCode=true matching official CodeGraph behavior.
	skipCode := true
	if args.SkipCode != nil {
		skipCode = *args.SkipCode
	}
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Query, args.Path); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)
	text, err := tools.ToolExplore(ctx, database, s.Workdirs, root, tools.ExploreArgs{
		Query:    args.Query,
		Path:     args.Path,
		Max:      args.Max,
		SkipCode: skipCode,
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

func (s *Server) toolCallees(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSymbolMax
	}
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name, args.Path); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)

	// Graph-first (official CodeGraph path).
	if text, ok, err := tools.ToolCalleesGraph(ctx, database, root, tools.GraphQueryArgs{
		Name: args.Name, Path: args.Path, File: args.File, Glob: args.Glob, MaxResults: args.MaxResults,
	}); err != nil {
		return nil, nil, err
	} else if ok {
		text = truncateOutput(text, defaultOutputChars)
		text = s.addStalenessWarning(text)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}

	// Fallback: body-parse via rg (legacy path in callees_fallback.go).
	return s.toolCalleesBodyFallback(ctx, root, args)
}

func (s *Server) toolCallers(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSymbolMax
	}
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name, args.Path); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)

	// Graph-first (official CodeGraph path).
	if text, ok, err := tools.ToolCallersGraph(ctx, database, root, tools.GraphQueryArgs{
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
		"--fixed-strings",
		fmt.Sprintf("--max-count=%d", rgCap),
		"-w", args.Name, searchRoot)
	if args.Glob != "" {
		rg.Args = append(rg.Args, "--glob", args.Glob)
	}
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no references found (index empty for this symbol; rg fallback also empty)"}}}, nil, nil
	}
	// Compile (or reuse) a regex that matches definitions of the target symbol.
	// The fixed prefix is the same for every name; only the quoted name varies.
	defRe := s.getCachedDefRe(args.Name)
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
		// Convert absolute path to relative for consistency with FTS/graph output.
		relFile := db.RelPath(root, parts[0])
		filtered = append(filtered, fmt.Sprintf("%s:%s:%s", relFile, parts[1], text))
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

func (s *Server) toolImpact(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultSymbolMax
	}
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name, args.Path); p != "" {
			args.ProjectPath = p
		}
	}
	projRoot, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(projRoot)

	// Graph BFS first (official getImpactRadius).
	if text, ok, err := tools.ToolImpactGraph(ctx, database, projRoot, tools.GraphQueryArgs{
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
		"--fixed-strings",
		"-c", "-w", args.Name, root)
	out, err := rg.Output()
	if err != nil && len(out) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no files reference " + args.Name}}}, nil, nil
	}
	rgText := relativizeRgOutput(string(out), projRoot)
	result := "# Impact of " + args.Name + " (rg fallback)\n" + limitLines(rgText, args.MaxResults)
	result = truncateOutput(result, defaultOutputChars)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

func (s *Server) toolNode(ctx context.Context, _ *mcp.CallToolRequest, args nodeArgs) (*mcp.CallToolResult, any, error) {
	if args.ProjectPath == "" {
		if p := s.detectProject(args.Name, args.File); p != "" {
			args.ProjectPath = p
		}
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)
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

func (s *Server) toolStatus(ctx context.Context, _ *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, any, error) {
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)
	var pendingFiles []string
	// Pending files only apply to the default session watcher.
	if root == s.Workdir {
		if w := s.Watcher.Load(); w != nil {
			pendingFiles = w.PendingFiles()
		}
	}

	result, err := tools.ToolStatus(ctx, database, s.Workdirs, root, tools.StatusArgs{
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

func (s *Server) toolAffected(ctx context.Context, _ *mcp.CallToolRequest, args affectedArgs) (*mcp.CallToolResult, any, error) {
	// Never read process stdin here — it is the MCP JSON-RPC stream.
	if args.Stdin {
		return nil, nil, fmt.Errorf("stdin is not supported over MCP; pass files as a list instead")
	}
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)
	result, err := tools.ToolAffected(ctx, database, root, tools.AffectedArgs{
		Files:  args.Files,
		Stdin:  false,
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

func (s *Server) toolCommunities(ctx context.Context, _ *mcp.CallToolRequest, args communitiesArgs) (*mcp.CallToolResult, any, error) {
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)
	result, err := tools.ToolCommunity(ctx, database, root, tools.CommunityArgs{
		Max:     args.Max,
		MinSize: args.MinSize,
	})
	if err != nil {
		return nil, nil, err
	}
	if result == nil || len(result.Content) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no community data found"}},
		}, nil, nil
	}
	text := truncateOutput(result.Content[0].Text, defaultOutputChars)
	text = s.addStalenessWarning(text)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

// ---------- store_fact ----------

func (s *Server) toolStoreFact(ctx context.Context, _ *mcp.CallToolRequest, args storeFactArgs) (*mcp.CallToolResult, any, error) {
	if args.TargetFile == "" {
		return nil, nil, fmt.Errorf("targetFile is required")
	}
	if args.Content == "" {
		return nil, nil, fmt.Errorf("content is required")
	}

	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)

	// Normalize absolute path to workdir-relative
	targetFile := args.TargetFile
	if filepath.IsAbs(targetFile) {
		rel, err := filepath.Rel(root, targetFile)
		if err == nil && !strings.HasPrefix(rel, "..") {
			targetFile = filepath.ToSlash(rel)
		}
	}

	// SHA-256 of content
	h := sha256.Sum256([]byte(args.Content))
	hash := hex.EncodeToString(h[:])

	// Check duplicate by hash
	existing, err := database.GetFactByHash(hash)
	if err != nil {
		return nil, nil, fmt.Errorf("check hash: %w", err)
	}
	if existing != nil {
		sameTargetFacts, _ := database.GetFactsByTarget(targetFile, args.TargetSymbol)
		out := map[string]interface{}{
			"duplicate":   true,
			"fact":        existing,
			"same_target": sameTargetFacts,
		}
		b, _ := json.Marshal(out)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, nil, nil
	}

	f := &db.Fact{
		TargetFile:   targetFile,
		TargetSymbol: args.TargetSymbol,
		TargetLine:   args.TargetLine,
		Content:      args.Content,
		ContentHash:  hash,
		Author:       args.Author,
		Status:       "active",
	}
	newID, err := database.InsertFact(f)
	if err != nil {
		return nil, nil, fmt.Errorf("insert fact: %w", err)
	}

	// Handle supersedes after insert so we can link the new ID
	var supersedeErr string
	if args.Supersedes > 0 {
		if err := database.SupersedeFact(args.Supersedes, newID); err != nil {
			supersedeErr = err.Error()
		}
	}

	inserted, _ := database.GetFactByHash(hash)
	sameTargetFacts, _ := database.GetFactsByTarget(targetFile, args.TargetSymbol)

	resp := map[string]interface{}{
		"duplicate":   false,
		"fact":        inserted,
		"same_target": sameTargetFacts,
	}
	if supersedeErr != "" {
		resp["supersede_warning"] = supersedeErr
	}

	b, _ := json.Marshal(resp)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}

// ---------- search_facts ----------

func (s *Server) toolSearchFacts(ctx context.Context, _ *mcp.CallToolRequest, args searchFactsArgs) (*mcp.CallToolResult, any, error) {
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)

	max := args.Max
	if max <= 0 {
		max = 20
	}
	status := args.Status
	if status == "" {
		status = "active"
	}

	facts, err := database.SearchFacts(args.Query, args.TargetFile, args.TargetSymbol, status, max)
	if err != nil {
		return nil, nil, fmt.Errorf("search facts: %w", err)
	}

	if len(facts) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no facts found"}},
		}, nil, nil
	}

	b, _ := json.Marshal(facts)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}

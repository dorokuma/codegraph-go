package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"

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
	Pattern     string `json:"pattern"      jsonschema:"literal text by default; set regex=true to treat pattern as a regular expression (ripgrep syntax)"`
	Path        string `json:"path,omitempty" jsonschema:"optional subdirectory under workspace,optional"`
	Glob        string `json:"glob,omitempty" jsonschema:"optional file glob filter, e.g. \"*.go\",optional"`
	MaxResults  int    `json:"max_results,omitempty" jsonschema:"global match cap (default 70; per-file also capped),optional"`
	IgnoreCase  bool   `json:"ignore_case,omitempty" jsonschema:"case-insensitive search,optional"`
	NoIgnore    bool   `json:"no_ignore,omitempty" jsonschema:"search inside .gitignore'd files (default false: ignore rules are respected),optional"`
	Regex       bool   `json:"regex,omitempty" jsonschema:"treat pattern as a regular expression (default false: literal match),optional"`
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
	Path        string `json:"path,omitempty"     jsonschema:"home-mode project selector: a project directory name under a broad workdir (e.g. \"myrepo\"); ignored for subdirectory scoping since community detection is graph-wide,optional"`
	ProjectPath string `json:"projectPath,omitempty" jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type storeFactArgs struct {
	TargetFile   string `json:"targetFile"             jsonschema:"file path the fact pertains to (required)"`
	TargetSymbol string `json:"targetSymbol,omitempty"  jsonschema:"symbol name within the file,optional"`
	TargetLine   int    `json:"targetLine,omitempty"    jsonschema:"line number within the file,optional"`
	Content      string `json:"content"                jsonschema:"fact content (required)"`
	Author       string `json:"author,omitempty"        jsonschema:"who wrote this fact,optional"`
	Supersedes   int64  `json:"supersedes,omitempty"    jsonschema:"fact id this replaces (creates a supersede chain),optional"`
	Path         string `json:"path,omitempty"         jsonschema:"home-mode project selector: a project directory name under a broad workdir (e.g. \"myrepo\"),optional"`
	ProjectPath  string `json:"projectPath,omitempty"  jsonschema:"absolute path to the project to write to (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

type searchFactsArgs struct {
	Query        string `json:"query,omitempty"       jsonschema:"search term in fact content (case-insensitive substring match),optional"`
	TargetFile   string `json:"targetFile,omitempty"   jsonschema:"filter by target file path,optional"`
	TargetSymbol string `json:"targetSymbol,omitempty" jsonschema:"filter by target symbol,optional"`
	Status       string `json:"status,omitempty"       jsonschema:"filter by status (default 'active'; pass 'all' for all statuses),optional"`
	Max          int    `json:"max,omitempty"           jsonschema:"max results (default 20),optional"`
	Path         string `json:"path,omitempty"         jsonschema:"home-mode project selector: a project directory name under a broad workdir (e.g. \"myrepo\"),optional"`
	ProjectPath  string `json:"projectPath,omitempty"  jsonschema:"absolute path to the project to query (or any directory inside it) — uses the nearest .codegraph/ index at or above that path. Omit for this session's default project.,optional"`
}

// codegraphArgs is the single MCP entry (action router). Same surface for every MCP host
// (Grok, Pi, etc.): one tool schema instead of N near-duplicate tools.
type codegraphArgs struct {
	Action       string   `json:"action" jsonschema:"Action to perform: explore|search|files|node|callers|callees|impact|status|affected|communities|store_fact|search_facts"`
	Pattern      string   `json:"pattern,omitempty" jsonschema:"search: literal by default (regex=true enables regex),optional"`
	Name         string   `json:"name,omitempty" jsonschema:"callees/callers/impact/node: symbol name,optional"`
	File         string   `json:"file,omitempty" jsonschema:"node/callees/callers/impact: file path or basename to pin,optional"`
	Query        string   `json:"query,omitempty" jsonschema:"explore/search_facts: free-text or search term,optional"`
	Path         string   `json:"path,omitempty" jsonschema:"subdirectory or home-mode project name,optional"`
	Glob         string   `json:"glob,omitempty" jsonschema:"search/files/callees/callers/impact: file glob,optional"`
	Max          int      `json:"max,omitempty" jsonschema:"result cap (search/files/explore/graph/communities/search_facts),optional"`
	MaxResults   int      `json:"max_results,omitempty" jsonschema:"alias of max for search/callers/callees/impact,optional"`
	IgnoreCase   bool     `json:"ignore_case,omitempty" jsonschema:"search: case-insensitive,optional"`
	NoIgnore     bool     `json:"no_ignore,omitempty" jsonschema:"search: include .gitignore'd files (default false),optional"`
	Regex        bool     `json:"regex,omitempty" jsonschema:"search: treat pattern as regex (default false: literal),optional"`
	Line         int      `json:"line,omitempty" jsonschema:"node: pin definition line,optional"`
	IncludeCode  *bool    `json:"includeCode,omitempty" jsonschema:"node: include source body (default false),optional"`
	SymbolsOnly  bool     `json:"symbolsOnly,omitempty" jsonschema:"node file mode: symbol map only,optional"`
	Offset       int      `json:"offset,omitempty" jsonschema:"node file mode: 1-based start line,optional"`
	Limit        int      `json:"limit,omitempty" jsonschema:"node file mode: max lines,optional"`
	SkipCode     *bool    `json:"skipCode,omitempty" jsonschema:"explore: omit source bodies (default true),optional"`
	Files        []string `json:"files,omitempty" jsonschema:"affected: changed source files,optional"`
	Depth        int      `json:"depth,omitempty" jsonschema:"affected: traversal depth,optional"`
	Filter       string   `json:"filter,omitempty" jsonschema:"affected: test file glob,optional"`
	MinSize      int      `json:"minSize,omitempty" jsonschema:"communities: min community size,optional"`
	TargetFile   string   `json:"targetFile,omitempty" jsonschema:"store_fact/search_facts: target file,optional"`
	TargetSymbol string   `json:"targetSymbol,omitempty" jsonschema:"store_fact/search_facts: target symbol,optional"`
	TargetLine   int      `json:"targetLine,omitempty" jsonschema:"store_fact: line,optional"`
	Content      string   `json:"content,omitempty" jsonschema:"store_fact: fact text,optional"`
	Author       string   `json:"author,omitempty" jsonschema:"store_fact: author,optional"`
	Supersedes   int64    `json:"supersedes,omitempty" jsonschema:"store_fact: fact id to replace,optional"`
	Status       string   `json:"status,omitempty" jsonschema:"search_facts: status filter,optional"`
	ProjectPath  string   `json:"projectPath,omitempty" jsonschema:"absolute path inside a project (nearest .codegraph/),optional"`
	Stdin        bool     `json:"stdin,omitempty" jsonschema:"affected: unsupported over MCP,optional"`
}

// codegraphActions lists valid action values for errors and docs.
var codegraphActions = []string{
	"explore", "search", "files", "node", "callers", "callees", "impact",
	"status", "affected", "communities", "store_fact", "search_facts",
}

// ---------- newMCPServer ----------

// NewMCPServer registers the single aggregated MCP tool and returns the server.
// Breaking (v0.8+): former top-level tools (explore, search, …) are only available
// as action= on tool "codegraph".
func NewMCPServer(s *Server) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "codegraph-go", Version: daemon.PackageVersion}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "codegraph",
		Description: "PRIMARY — single entry for code search & call-graph analysis (low tool-list noise). " +
			"Pass action= to select capability. Prefer action=explore FIRST for almost any question or before an edit " +
			"(query= symbol bag or free text; empty query = overview). " +
			"Actions: explore, search(pattern), files(pattern), node(file|name), callers/callees/impact(name), " +
			"status, affected(files), communities, store_fact, search_facts. " +
			"Common: path (home-mode project), projectPath (absolute), max, glob. " +
			"explore skipCode defaults true (no bodies); set skipCode=false or node includeCode=true to read source.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"action": {
					Type:        "string",
					Description: "explore|search|files|node|callers|callees|impact|status|affected|communities|store_fact|search_facts",
					Enum:        []interface{}{"explore", "search", "files", "node", "callers", "callees", "impact", "status", "affected", "communities", "store_fact", "search_facts"},
				},
				"pattern":      {Type: "string", Description: "search: literal by default (regex=true enables regex); files: glob pattern"},
				"name":         {Type: "string", Description: "symbol name for node/callers/callees/impact"},
				"file":         {Type: "string", Description: "file path/basename pin for node/graph actions"},
				"query":        {Type: "string", Description: "explore/search_facts free text"},
				"path":         {Type: "string", Description: "subdir or home-mode project name"},
				"glob":         {Type: "string", Description: "file glob filter"},
				"max":          {Type: "integer", Description: "result cap"},
				"max_results":  {Type: "integer", Description: "alias of max"},
				"ignore_case":  {Type: "boolean", Description: "search: case-insensitive"},
				"no_ignore":    {Type: "boolean", Description: "search: include .gitignore'd files (default false)"},
				"regex":        {Type: "boolean", Description: "search: treat pattern as regex (default false: literal)"},
				"line":         {Type: "integer", Description: "node: pin line"},
				"includeCode":  {Type: "boolean", Description: "node: include body"},
				"symbolsOnly":  {Type: "boolean", Description: "node file mode: map only"},
				"offset":       {Type: "integer", Description: "node file mode start line"},
				"limit":        {Type: "integer", Description: "node file mode max lines"},
				"skipCode":     {Type: "boolean", Description: "explore: omit bodies (default true)"},
				"files":        {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "affected: changed sources"},
				"depth":        {Type: "integer", Description: "affected depth"},
				"filter":       {Type: "string", Description: "affected test glob"},
				"minSize":      {Type: "integer", Description: "communities min size"},
				"targetFile":   {Type: "string", Description: "store_fact/search_facts file"},
				"targetSymbol": {Type: "string", Description: "store_fact/search_facts symbol"},
				"targetLine":   {Type: "integer", Description: "store_fact line"},
				"content":      {Type: "string", Description: "store_fact content"},
				"author":       {Type: "string", Description: "store_fact author"},
				"supersedes":   {Type: "integer", Description: "store_fact supersede id"},
				"status":       {Type: "string", Description: "search_facts status filter"},
				"projectPath":  {Type: "string", Description: "absolute path inside project"},
			},
			Required: []string{"action"},
		},
	}, s.toolCodegraph)

	return srv
}

// toolCodegraph routes action= to the internal handlers (same implementations as pre-0.8 tools).
// The deferred recover is the tool-dispatch panic barrier (audit critical): a
// panic inside any action (e.g. a bad slice index from a hostile max) must
// become a single-call MCP error — never a crash of the shared daemon process
// that serves every session. The daemon's serveConn recover catches panics
// outside the tool path (transport/hello).
func (s *Server) toolCodegraph(ctx context.Context, req *mcp.CallToolRequest, args codegraphArgs) (res *mcp.CallToolResult, out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tool panic (action=%q): %v\n%s", args.Action, r, debug.Stack())
			res, out = nil, nil
			err = fmt.Errorf("codegraph tool %q panicked: %v", args.Action, r)
		}
	}()
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		return nil, nil, fmt.Errorf("action is required (one of: %s)", strings.Join(codegraphActions, ", "))
	}
	// max preferred; max_results accepted as alias (legacy multi-tool clients).
	cap := args.Max
	if cap == 0 {
		cap = args.MaxResults
	}

	switch action {
	case "explore":
		return s.toolExplore(ctx, req, exploreArgs{
			Query: args.Query, Path: args.Path, Max: cap, SkipCode: args.SkipCode, ProjectPath: args.ProjectPath,
		})
	case "search":
		return s.toolSearch(ctx, req, searchArgs{
			Pattern: args.Pattern, Path: args.Path, Glob: args.Glob, MaxResults: cap,
			IgnoreCase: args.IgnoreCase, NoIgnore: args.NoIgnore, Regex: args.Regex,
			ProjectPath: args.ProjectPath,
		})
	case "files":
		pattern := args.Pattern
		if pattern == "" && args.Glob != "" {
			pattern = args.Glob
		}
		return s.toolFiles(ctx, req, filesArgs{
			Pattern: pattern, Path: args.Path, Max: cap, ProjectPath: args.ProjectPath,
		})
	case "node":
		return s.toolNode(ctx, req, nodeArgs{
			Name: args.Name, File: args.File, Line: args.Line, IncludeCode: args.IncludeCode,
			SymbolsOnly: args.SymbolsOnly, Offset: args.Offset, Limit: args.Limit, ProjectPath: args.ProjectPath,
		})
	case "callers":
		return s.toolCallers(ctx, req, nameArgs{
			Name: args.Name, File: args.File, Path: args.Path, Glob: args.Glob,
			MaxResults: cap, ProjectPath: args.ProjectPath,
		})
	case "callees":
		return s.toolCallees(ctx, req, nameArgs{
			Name: args.Name, File: args.File, Path: args.Path, Glob: args.Glob,
			MaxResults: cap, ProjectPath: args.ProjectPath,
		})
	case "impact":
		return s.toolImpact(ctx, req, nameArgs{
			Name: args.Name, File: args.File, Path: args.Path, Glob: args.Glob,
			MaxResults: cap, ProjectPath: args.ProjectPath,
		})
	case "status":
		return s.toolStatus(ctx, req, statusArgs{Path: args.Path, ProjectPath: args.ProjectPath})
	case "affected":
		return s.toolAffected(ctx, req, affectedArgs{
			Files: args.Files, Stdin: args.Stdin, Depth: args.Depth, Filter: args.Filter, ProjectPath: args.ProjectPath,
		})
	case "communities":
		return s.toolCommunities(ctx, req, communitiesArgs{
			Max: cap, MinSize: args.MinSize, Path: args.Path, ProjectPath: args.ProjectPath,
		})
	case "store_fact":
		return s.toolStoreFact(ctx, req, storeFactArgs{
			TargetFile: args.TargetFile, TargetSymbol: args.TargetSymbol, TargetLine: args.TargetLine,
			Content: args.Content, Author: args.Author, Supersedes: args.Supersedes,
			Path: args.Path, ProjectPath: args.ProjectPath,
		})
	case "search_facts":
		return s.toolSearchFacts(ctx, req, searchFactsArgs{
			Query: args.Query, TargetFile: args.TargetFile, TargetSymbol: args.TargetSymbol,
			Status: args.Status, Max: cap, Path: args.Path, ProjectPath: args.ProjectPath,
		})
	default:
		return nil, nil, fmt.Errorf("unknown action %q; want one of: %s", args.Action, strings.Join(codegraphActions, ", "))
	}
}

// homeModeRefuseRg is returned when a home-mode session would otherwise pass
// the entire $HOME / workdir to rg. Explicit path= (including .ssh) still
// searches that subdirectory via resolvePathIn.
const homeModeRefuseRg = "home-mode: pass path= or projectPath= to search a tree; refusing to ripgrep the whole home"

// rgSearchRoots chooses directories to pass to rg. An explicit path is always
// resolved in-jail (intentional search of a subdirectory, including .ssh).
// When path is empty and projRoot is a broad workdir, never return the home
// itself: return cached DetectDirs, or refuse=true if none.
func (s *Server) rgSearchRoots(projRoot, path string) (roots []string, refuse bool, err error) {
	if path != "" {
		r, err := s.resolvePathIn(projRoot, path)
		if err != nil {
			return nil, false, err
		}
		return []string{r}, false, nil
	}
	if extraction.IsBroadWorkdir(projRoot) {
		dirs := s.detectedProjectDirs()
		if len(dirs) == 0 {
			return nil, true, nil
		}
		return dirs, false, nil
	}
	r, err := s.resolvePathIn(projRoot, "")
	if err != nil {
		return nil, false, err
	}
	return []string{r}, false, nil
}

// ---------- tool implementations ----------

func (s *Server) toolSearch(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Pattern == "" {
		return nil, nil, fmt.Errorf("pattern is required")
	}
	// Clamp the global cap: negative values would panic downstream slice
	// operations, absurd values would amplify memory/CPU (audit critical #2,
	// H2). The final payload is truncated to defaultOutputChars regardless.
	args.MaxResults = clampLimit(args.MaxResults, defaultSearchGlobal, maxSearchResults)
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path})
	projRoot, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(projRoot)

	// Official CodeGraph search is symbol-first. For a plain identifier with no
	// path/glob/regex metacharacters, hit FTS before spawning rg.
	// no_ignore=true must not take this shortcut: FTS only sees indexed
	// (non-ignored) files, so --no-ignore has to go through rg.
	if args.Path == "" && args.Glob == "" && !args.IgnoreCase && !args.Regex && !args.NoIgnore && isSimpleIdent(args.Pattern) {
		nodes, err := database.FullTextSearchRefsContext(ctx, args.Pattern, args.MaxResults)
		if err == nil && len(nodes) > 0 {
			var b strings.Builder
			for _, n := range nodes {
				fmt.Fprintf(&b, "%s:%d\n", db.RelPath(projRoot, n.File), matchLineForNode(projRoot, n, args.Pattern))
			}
			text := truncateOutput(b.String(), defaultOutputChars)
			text = s.addStalenessWarning(text)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
			}, nil, nil
		}
	}

	roots, refuse, err := s.rgSearchRoots(projRoot, args.Path)
	if err != nil {
		return nil, nil, err
	}
	if refuse {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: homeModeRefuseRg}},
		}, nil, nil
	}
	perFile := searchPerFileCap(args.MaxResults)
	outLines, err := rgEachRoot(roots, args.MaxResults, rgMaxOutputBytes, func(searchRoot string) *exec.Cmd {
		rg := exec.CommandContext(ctx, "rg",
			"--line-number", "--no-heading", "--color=never",
			fmt.Sprintf("--max-count=%d", perFile),
		)
		if args.IgnoreCase {
			rg.Args = append(rg.Args, "-i")
		}
		// Literal matching by default: a caller pattern like "a(b" must search
		// for that text, not fail as an invalid regex or burn CPU on a crafted
		// expression. Only the explicit regex=true flag enables regex semantics
		// (audit M1).
		if !args.Regex {
			rg.Args = append(rg.Args, "--fixed-strings")
		}
		if args.Glob != "" {
			rg.Args = append(rg.Args, "--glob", args.Glob)
		}
		// Respect .gitignore unless the caller explicitly opts out: ignoring a
		// path's ignore rules would surface .env/private keys into the agent
		// (audit M2). no_ignore=true re-enables the old sweep.
		if args.NoIgnore {
			rg.Args = append(rg.Args, "--no-ignore")
		}
		rg.Args = append(rg.Args, "--", args.Pattern, searchRoot)
		return rg
	})
	if err != nil {
		return nil, nil, fmt.Errorf("rg search: %w", err)
	}
	if len(outLines) == 0 {
		// When a path subdirectory is specified, the user may be searching
		// an unindexed area. Check whether any files are indexed under root
		// and include a hint so the agent knows to use built-in tools.
		msg := "no matches"
		if args.Path != "" && len(roots) == 1 {
			indexed, cerr := countIndexedUnder(ctx, database, projRoot, roots[0])
			if cerr == nil && indexed == 0 {
				msg = fmt.Sprintf("no matches; path %q may not be indexed (0 indexed files under %s)", args.Path, db.RelPath(projRoot, roots[0]))
			}
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		}, nil, nil
	}
	text := relativizeRgOutput(strings.Join(outLines, "\n")+"\n", projRoot)
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
	// Clamp: a negative max previously reached lines[:args.Max] and panicked
	// (audit critical #2); an unbounded max would load the whole rg output.
	args.Max = clampLimit(args.Max, defaultFilesMax, maxFilesResults)
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path})
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
			Content: []mcp.Content{&mcp.TextContent{Text: truncateOutput(text, defaultOutputChars)}},
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
	lines, _, err := rgOutputLines(rg, args.Max, rgMaxOutputBytes)
	if err != nil {
		if len(lines) == 0 {
			return nil, nil, fmt.Errorf("rg files: %w", err)
		}
	}
	if len(lines) == 0 {
		// rg exits 1 when nothing matched; other failures surface above.
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no files matched"}},
		}, nil, nil
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Default skipCode=true matching official CodeGraph behavior.
	skipCode := true
	if args.SkipCode != nil {
		skipCode = *args.SkipCode
	}
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path})
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)
	// B6: explore's path filter must pass the same symlink-aware jail check as
	// search/files — ToolExplore's own prefix test is not symlink-aware.
	if args.Path != "" {
		resolvedPath, perr := s.resolvePathIn(root, args.Path)
		if perr != nil {
			return nil, nil, perr
		}
		args.Path = resolvedPath
	}
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
	args.MaxResults = clampLimit(args.MaxResults, defaultSymbolMax, maxSymbolResults)
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path})
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
	// Home-mode with empty path must not ripgrep the whole home.
	rgRoots, refuse, rerr := s.rgSearchRoots(root, args.Path)
	if rerr != nil {
		return nil, nil, rerr
	}
	if refuse {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no call edges in the graph; " + homeModeRefuseRg}},
		}, nil, nil
	}
	return s.toolCalleesBodyFallback(ctx, root, database, args, rgRoots...)
}

func (s *Server) toolCallers(ctx context.Context, _ *mcp.CallToolRequest, args nameArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.Name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	args.MaxResults = clampLimit(args.MaxResults, defaultSymbolMax, maxSymbolResults)
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path})
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
	searchRoots, refuse, err := s.rgSearchRoots(root, args.Path)
	if err != nil {
		return nil, nil, err
	}
	if refuse {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no call edges in the graph; " + homeModeRefuseRg}},
		}, nil, nil
	}
	rgCap := args.MaxResults * 3
	if rgCap > 200 {
		rgCap = 200
	}
	outLines, err := rgEachRoot(searchRoots, rgCap, rgMaxOutputBytes, func(searchRoot string) *exec.Cmd {
		rg := exec.CommandContext(ctx, "rg",
			"--line-number", "--no-heading", "--color=never",
			"--fixed-strings",
			fmt.Sprintf("--max-count=%d", rgCap),
			"-w")
		if args.Glob != "" {
			rg.Args = append(rg.Args, "--glob", args.Glob)
		}
		// "--" ends flag parsing so a Name starting with "-" is matched
		// literally instead of parsed as an rg flag (flag injection).
		rg.Args = append(rg.Args, "--", args.Name, searchRoot)
		return rg
	})
	if err != nil {
		if len(outLines) == 0 {
			return nil, nil, fmt.Errorf("rg callers fallback: %w", err)
		}
	}
	// Compile (or reuse) a regex that matches definitions of the target symbol.
	// The fixed prefix is the same for every name; only the quoted name varies.
	defRe := s.getCachedDefRe(args.Name)
	var filtered []string
	for _, line := range outLines {
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
	args.MaxResults = clampLimit(args.MaxResults, defaultSymbolMax, maxSymbolResults)
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path})
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

	searchRoots, refuse, err := s.rgSearchRoots(projRoot, args.Path)
	if err != nil {
		return nil, nil, err
	}
	if refuse {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no call edges in the graph; " + homeModeRefuseRg}},
		}, nil, nil
	}
	outLines, err := rgEachRoot(searchRoots, args.MaxResults, rgMaxOutputBytes, func(searchRoot string) *exec.Cmd {
		return exec.CommandContext(ctx, "rg",
			"--line-number", "--no-heading", "--color=never",
			"--fixed-strings", "-c", "-w",
			// "--" ends flag parsing so a Name starting with "-" is matched
			// literally instead of parsed as an rg flag (flag injection).
			"--", args.Name, searchRoot)
	})
	if err != nil {
		if len(outLines) == 0 {
			return nil, nil, fmt.Errorf("rg impact fallback: %w", err)
		}
	}
	rgText := relativizeRgOutput(strings.Join(outLines, "\n")+"\n", projRoot)
	result := "# Impact of " + args.Name + " (rg fallback)\n" + limitLines(rgText, args.MaxResults)
	result = truncateOutput(result, defaultOutputChars)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
}

func (s *Server) toolNode(ctx context.Context, _ *mcp.CallToolRequest, args nodeArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.File})
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)
	var pendingFiles []string
	var dropped uint64
	// Pending files and the permanent-drop count only apply to the default
	// session watcher.
	if root == s.Workdir {
		if w := s.Watcher.Load(); w != nil {
			pendingFiles = w.PendingFiles()
			dropped = w.DroppedCount()
		}
	}

	result, err := tools.ToolStatus(ctx, database, s.Workdirs, root, tools.StatusArgs{
		Path: args.Path,
	}, pendingFiles, dropped)
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
	// Louvain on a large index is the heaviest tool: give it a longer
	// deadline than the default 30s, but still bound it (audit high: full-
	// graph community detection had no timeout at all). The node-count cap
	// lives in tools.ToolCommunity — the real protection against CPU burn.
	// Since ToolCommunity polls ctx through the graph-build and enrichment
	// loops, this deadline now actually aborts a canceled run mid-build
	// instead of only at the Louvain gates; the non-interruptible remainder
	// is the DB snapshot SELECT and the gonum Louvain/Q passes themselves.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args.Max = clampLimit(args.Max, 20, maxCommunities)
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path})
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args.TargetFile == "" {
		return nil, nil, fmt.Errorf("targetFile is required")
	}
	if args.Content == "" {
		return nil, nil, fmt.Errorf("content is required")
	}
	// Bound the fact size before it reaches the DB: an unbounded content
	// would let any caller grow the facts table into a disk DoS and make
	// every read-back path slow (audit high M9).
	if len(args.Content) > maxFactContentLen {
		return nil, nil, fmt.Errorf("content too large: %d bytes (max %d)", len(args.Content), maxFactContentLen)
	}

	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path, &args.TargetFile})
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)

	targetFile, nerr := s.normalizeFactTargetFile(root, args.TargetFile)
	if nerr != nil {
		return nil, nil, nerr
	}

	// SHA-256 of content
	h := sha256.Sum256([]byte(args.Content))
	hash := hex.EncodeToString(h[:])

	// Duplicate only when this exact target already has the same text.
	existing, err := database.GetFactByHashAndTarget(hash, targetFile, args.TargetSymbol)
	if err != nil {
		return nil, nil, fmt.Errorf("check hash: %w", err)
	}
	if existing != nil {
		sameTarget, gerr := database.GetFactsByTarget(targetFile, args.TargetSymbol)
		if gerr != nil {
			// Read-back failure is non-fatal: still report the duplicate.
			sameTarget = nil
		}
		return s.storeFactResponse(existing, sameTarget, map[string]interface{}{"duplicate": true})
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
	if _, err := database.InsertFactSuperseding(f, args.Supersedes); err != nil {
		return nil, nil, fmt.Errorf("insert fact: %w", err)
	}

	inserted, _ := database.GetFactByHashAndTarget(hash, targetFile, args.TargetSymbol)
	sameTargetFacts, _ := database.GetFactsByTarget(targetFile, args.TargetSymbol)

	resp := map[string]interface{}{
		"duplicate": false,
	}

	return s.storeFactResponse(inserted, sameTargetFacts, resp)
}

// normalizeFactTargetFile confines targetFile to root (lexical jail + realpath)
// and returns the workdir-relative storage key used by facts. Store and search
// must share this so an absolute or dirty path finds the same row.
func (s *Server) normalizeFactTargetFile(root, raw string) (string, error) {
	// Two-layered, mirroring resolvePathIn: a lexical jail (absolute paths
	// outside the root and relative ../ escapes are rejected — a fact must
	// never target a file outside the project) and a real-path check (a
	// symlink inside the project pointing outside is an escape too). Real
	// path handling follows the deepest existing ancestor, so a not-yet-
	// created target file (a legal future file) stays allowed as long as its
	// existing prefix does not escape.
	targetFile := filepath.Clean(raw)
	if !filepath.IsAbs(targetFile) {
		targetFile = filepath.Clean(filepath.Join(root, targetFile))
	}
	if rel, rerr := filepath.Rel(root, targetFile); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("targetFile %q is outside the project root %q", raw, s.displayRoot(root))
	}
	realTarget, rerr := filepath.EvalSymlinks(targetFile)
	if rerr != nil {
		realTarget, rerr = resolveExistingAncestor(targetFile)
		if rerr != nil {
			return "", fmt.Errorf("targetFile %q cannot be resolved: %v", raw, rerr)
		}
	}
	if !pathWithinRealRoot(s.realRoot(root), realTarget) {
		return "", fmt.Errorf("targetFile %q resolves to %q outside the project root %q (symlink escape)", raw, s.displayPath(realTarget), s.displayRoot(root))
	}
	if rel, rerr := filepath.Rel(root, targetFile); rerr == nil && rel != "." {
		return filepath.ToSlash(rel), nil
	} else if rerr == nil {
		return ".", nil
	}
	return filepath.ToSlash(targetFile), nil
}

// storeFactResponse marshals the store_fact JSON payload with hard bounds:
// same_target facts are limited to maxFactsReadback rows and each fact's
// content is truncated to maxFactContentShown bytes before marshaling
// (read-back LIMIT + cap; audit high: GetFactsByTarget has no LIMIT and the
// response was not truncated). Extra rows are summarized, not dropped
// silently.
func (s *Server) storeFactResponse(fact *db.Fact, sameTarget []db.Fact, extra map[string]interface{}) (*mcp.CallToolResult, any, error) {
	resp := make(map[string]interface{}, len(extra)+2)
	for k, v := range extra {
		resp[k] = v
	}
	if fact != nil {
		f := *fact
		f.Content = truncateFactContent(f.Content)
		resp["fact"] = f
	}
	type factView struct {
		db.Fact
	}
	var shown []factView
	for i, f := range sameTarget {
		if i >= maxFactsReadback {
			break
		}
		f.Content = truncateFactContent(f.Content)
		shown = append(shown, factView{Fact: f})
	}
	if len(sameTarget) > len(shown) {
		resp["same_target_truncated"] = len(sameTarget) - len(shown)
	}
	resp["same_target"] = shown
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, nil, err
	}
	text := truncateOutput(string(b), defaultOutputChars)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

// truncateFactContent cuts fact content echoed back to the client to
// maxFactContentShown bytes so responses stay small even for max-size facts.
func truncateFactContent(content string) string {
	if len(content) <= maxFactContentShown {
		return content
	}
	cut := maxFactContentShown
	for cut > 0 && !utf8.ValidString(content[:cut]) {
		cut--
	}
	return content[:cut] + "… (fact content truncated)"
}

// ---------- search_facts ----------

func (s *Server) toolSearchFacts(ctx context.Context, _ *mcp.CallToolRequest, args searchFactsArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.adoptDetectedProject(&args.ProjectPath, []*string{&args.Path, &args.TargetFile})
	root, database, err := s.resolveProject(args.ProjectPath)
	if err != nil {
		return recoverableProjectErr(err)
	}
	defer s.releaseProject(root)

	// Clamp the row cap: the DB LIMIT takes this value verbatim (audit H2),
	// and the JSON payload used to be marshaled unbounded (audit H3).
	max := clampLimit(args.Max, 20, maxFactsResults)
	status := args.Status
	if status == "" {
		status = "active"
	}

	targetFile := args.TargetFile
	if targetFile != "" {
		norm, nerr := s.normalizeFactTargetFile(root, targetFile)
		if nerr != nil {
			return nil, nil, nerr
		}
		targetFile = norm
	}

	facts, err := database.SearchFacts(args.Query, targetFile, args.TargetSymbol, status, max)
	if err != nil {
		return nil, nil, fmt.Errorf("search facts: %w", err)
	}

	if len(facts) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no facts found"}},
		}, nil, nil
	}

	// Truncate each fact's content before marshaling so the intermediate
	// JSON is bounded, then truncate the payload again as the final cap.
	for i := range facts {
		facts[i].Content = truncateFactContent(facts[i].Content)
	}
	b, _ := json.Marshal(facts)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: truncateOutput(string(b), defaultOutputChars)}},
	}, nil, nil
}

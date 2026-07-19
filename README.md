# codegraph-go

A Go MCP server for code intelligence with SQLite indexing and auto-sync.

Based on [colbymchenry/codegraph](https://github.com/colbymchenry/codegraph) — official 8 MCP tools + `affected` extension.

Current version: **0.5.3** (alignment in progress). Index logic version **15**.

Pipeline: extract → park cross-file refs → `ResolveAll` → scrub pure-noise failed refs → `SynthesizeAll` (callback / React / JSX / bridge / C fn-pointer / GoFrame). Nodes carry qualified_name / signature / visibility / is_exported / return_type. Vue/Svelte/Astro SFCs get a file component + script/frontmatter + template component refs. IndexAll uses a file-level worker pool (`CODEGRAPH_INDEX_WORKERS`). Optional shared daemon (one writer per project, N thin stdio proxies). Logic bumps trigger a full rebuild.

## Features

Alignment: steps **1–9** done incl. 7.5 (logic **15**, **243** tests). Not full feature-parity — see `/root/codegraph-go-comparison.md` (next: step 10 eval).

- **9 MCP tools:** explore (PRIMARY), node (SECONDARY dual-mode), search, callers, callees, impact, files, status, affected (extension). `context` / `trace` / `search_fts` removed from MCP.
- **node dual mode:** `file` alone = Read-like numbered source + dependents; `name` = body + trail; overloads return every body in one call
- **projectPath on every tool:** walk up to nearest `.codegraph/` and query that project’s index (no cross-project DB bleed)
- **Graph-first queries:** callers / callees / impact walk the SQLite call graph (rg only as labeled fallback); optional `file` pins overloads
- **Extract → unresolved_refs → ResolveAll → scrub → SynthesizeAll:** cross-file calls parked then linked; pure framework noise without a project symbol is scrubbed
- **Path aliases / monorepo:** tsconfig/jsconfig `paths`, `@/` fallbacks, package/pnpm workspaces, go.mod module+replace, Cargo workspace members
- **Dynamic-dispatch edges:** callback / EventEmitter / React-render / JSX-child / bridge / C fn-pointer / GoFrame route (heuristic + synthesizedBy)
- **Smart explore:** Flow path + source for a bag of symbols; size-tier output budget
- **SQLite indexing:** symbols, edges, files, unresolved_refs in `.codegraph/codegraph.db`
- **FTS5 full-text search:** plain identifiers in `search` hit FTS first (no separate search_fts tool)
- **Tree-sitter AST parsing:** Go / TypeScript / JavaScript / Python with qualified_name, signature, is_exported, visibility, return_type (syntax-keyword call filter only)
- **SFC support:** Vue / Svelte / Astro — file component, multi-script/frontmatter, opening-tag template refs (HTML natives skipped, kebab→Pascal, `@click`/`v-on`)
- **Parallel index:** IndexAll worker pool (`CODEGRAPH_INDEX_WORKERS`, default min(8, cores-1)); DB writes serialized
- **Noise rules:** `ShouldParkRef` keeps real symbols even if named like `emit`; scrub after resolve
- **Regex fallback:** Rust (use/fn/impl + pub/signature + cargo map), Java, C#, Ruby, PHP, C, C++, Swift, Kotlin, Scala, Dart, Lua, Luau, R, Objective-C, Liquid, Pascal/Delphi
- **Framework route detection:** Gin, chi, gorilla/mux, Express, NestJS, Flask, FastAPI, Django, Laravel, Rails, Spring, ASP.NET, Axum, actix, Rocket, Vapor, Play
- **Cross-language bridging:** CGo (Go↔C), Python ctypes/cffi/Cython, React Native/Expo, Swift↔ObjC
- **Home-mode indexing:** workdir=`$HOME` only enters project-like top-level dirs
- **Shared daemon (optional):** one process per project root owns SQLite + watcher; MCP hosts attach via Unix socket proxy. `CODEGRAPH_NO_DAEMON=1` keeps the old embedded mode. Idle exit default 300s (`CODEGRAPH_DAEMON_IDLE_TIMEOUT_MS`).
- **content_hash incremental:** SHA-256 of file bytes; unchanged content skips re-extract even if mtime moved
- **Git-assist sync:** after cold index, `git status` picks up edits missed while nothing was watching (no hooks installed)
- **Auto-sync:** file watcher with 2-second debounce; new directories are watched recursively
- **Staleness warning:** warns when referenced files are pending sync
- **Respects .gitignore:** uses ripgrep for file operations


## Progress

Aligned steps **1–9** (including optional **7.5** C fn-pointer + GoFrame synthesis). Remaining: **step 10** evaluation/docs wrap-up.

| Item | Value |
|------|-------|
| Display version | 0.5.3 |
| Index logic | 15 |
| Tests | 243 passed |
| Feature parity | **not claimed** (step 10 open) |

Single source of truth: `/root/codegraph-go-comparison.md`.

## Installation

```bash
# Prerequisites
which rg        # ripgrep must be on PATH
go version      # Go 1.25+

# Build
cd codegraph-go
go build -o codegraph-go .

# Install
cp codegraph-go /usr/local/bin/codegraph-go
```

## Usage

### Standalone (stdio)

```bash
codegraph-go -workdir /path/to/project
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-workdir` | current directory | Workspace root |
| `-no-sync` | false | Disable auto-sync file watcher |

## MCP Tools

All tools accept optional `projectPath` (absolute path inside a project). The server walks up to the nearest `.codegraph/` and opens that index. Omit it to use the session default workdir.

| Tool | Purpose |
|------|---------|
| `explore` | **PRIMARY.** Overview or `query=` bag of names → Flow + source (treat as already Read) |
| `node` | **SECONDARY.** `file` alone = Read-like source + dependents; `name` = body + trail (all overloads) |
| `search` | Symbol FTS (simple names) or ripgrep (regex/path/glob) |
| `callers` | Who calls this symbol (graph first, rg fallback); optional `file` pin |
| `callees` | What it calls (graph first, body-parse fallback); optional `file` pin |
| `impact` | Blast radius via call-graph BFS; optional `file` pin |
| `files` | List files matching a glob pattern |
| `status` | Index health: node/edge/file counts, pending sync |
| `affected` | **Extension.** Find test files affected by changed source files |

## Indexing

On first run, codegraph-go indexes the entire project. The index includes:
- **Nodes:** functions, methods, classes, structs, interfaces, variables, constants
- **Edges:** calls, imports, extends, implements

The file watcher automatically re-indexes changed files within 2 seconds.

### Tree-sitter vs Regex

For Go, TypeScript, JavaScript, and Python, codegraph-go uses tree-sitter for accurate AST-based extraction. This provides:
- Better method detection (including struct methods with receivers)
- More accurate call graph (handles method calls, selector expressions)
- Proper handling of nested functions and closures

For other languages, regex-based extraction is used as a fallback.

### Framework Route Detection

codegraph-go detects web framework routing patterns and creates `route` nodes:

- **Go:** Gin, chi, gorilla/mux
- **JavaScript:** Express
- **Python:** Flask, FastAPI, Django

Routes are stored as nodes with kind `route`, where the name is `METHOD /path` and the body is the handler function name.

### Cross-language Bridging

codegraph-go detects cross-language calls and creates bridge edges:

- **CGo:** Go ↔ C via `import "C"` and `C.functionName()`
- **Python ctypes:** Python → C via `ctypes.CDLL()`
- **Python cffi:** Python → C via `ffi.cdef()` and `ffi.dlopen()`
- **Python Cython:** Python → C via `cdef extern from`
- **React Native:** JS → Native via `NativeModules` and TurboModules
- **Expo:** JS → Native via `requireNativeModule()`

## Architecture

```
codegraph-go/
├── main.go              # MCP server + 12 tools + projectPath routing
├── db/
│   ├── schema.sql       # SQLite schema
│   ├── connection.go    # Database connection
│   ├── root.go          # FindNearestCodeGraphRoot
│   └── query.go         # nodes, edges, files, unresolved_refs
├── extraction/
│   ├── common.go        # Language detection
│   ├── extractor.go     # Regex extraction (+ Rust use/fn)
│   ├── treesitter.go    # Tree-sitter AST (Go, TS, JS, Python)
│   ├── frameworks.go    # Framework route detection
│   ├── bridge.go        # Cross-language bridging
│   └── orchestrator.go  # Index builder → ResolveAll
├── resolution/
│   ├── resolver.go      # pending refs → edges
│   ├── name_matcher.go
│   ├── import_resolver.go
│   ├── path_aliases.go  # tsconfig/jsconfig paths
│   ├── go_module.go     # go.mod module + replace
│   ├── workspace_packages.go
│   └── cargo_workspace.go
├── sync/
│   └── watcher.go       # File watcher with debounce
├── testdata/parity/     # go, ts, py, alias, gomod, cargo, synth_*
└── tools/
    ├── graph.go         # explore / callers / callees / impact
    ├── node.go
    ├── status.go
    └── affected.go
```

## License

MIT

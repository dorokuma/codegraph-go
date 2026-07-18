# codegraph-go

A Go MCP server for code intelligence with SQLite indexing and auto-sync.

Based on [colbymchenry/codegraph](https://github.com/colbymchenry/codegraph) — provides 12 tools for AI coding agents.

## Features

- **12 MCP tools:** search, search_fts, files, context, explore, callees, callers, trace, impact, node, status, affected
- **SQLite indexing:** symbols, edges, and files stored in `.codegraph/codegraph.db`
- **FTS5 full-text search:** fast symbol search using SQLite FTS5
- **Tree-sitter AST parsing:** accurate symbol extraction for Go, TypeScript, JavaScript, Python
- **Regex fallback:** for other languages (Rust, Java, C#, Ruby, PHP, C, C++, Swift, Kotlin, Scala, Dart, Lua, Luau, R, Objective-C, Svelte, Vue, Astro, Liquid, Pascal/Delphi)
- **Framework route detection:** Gin, chi, gorilla/mux, Express, NestJS, Flask, FastAPI, Django, Laravel, Rails, Spring, ASP.NET, Axum, actix, Rocket, Vapor, Play
- **Cross-language bridging:** CGo (Go↔C), Python ctypes/cffi/Cython, React Native/Expo, Swift↔ObjC
- **Auto-sync:** file watcher with 2-second debounce, index stays fresh as you code
- **Staleness warning:** warns when referenced files are pending sync
- **Respects .gitignore:** uses ripgrep for file operations

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

| Tool | Purpose |
|------|---------|
| `search` | Text/regex search across the workspace (ripgrep) |
| `search_fts` | Full-text search over indexed symbols (SQLite FTS5) |
| `files` | List files matching a glob pattern |
| `context` | Read code around a file:line position |
| `explore` | Project overview: top-level dirs, READMEs, manifests |
| `callees` | List functions that a symbol calls |
| `callers` | Find all references to a symbol |
| `trace` | Grep with optional ±5 lines of context |
| `impact` | Per-file match count for a symbol |
| `node` | Get symbol details: source, callers, callees |
| `status` | Index health: node/edge/file counts, pending sync |
| `affected` | Find test files affected by changed source files |

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
├── main.go              # MCP server + 12 tools
├── db/
│   ├── schema.sql       # SQLite schema
│   ├── connection.go    # Database connection
│   └── query.go         # Query layer (nodes, edges, files)
├── extraction/
│   ├── common.go        # Language detection
│   ├── extractor.go     # Regex-based symbol extraction (fallback)
│   ├── treesitter.go    # Tree-sitter AST extraction (Go, TS, JS, Python)
│   ├── frameworks.go    # Framework route detection
│   ├── bridge.go        # Cross-language bridging
│   └── orchestrator.go  # Index builder
├── sync/
│   └── watcher.go       # File watcher with debounce
└── tools/
    ├── node.go          # Symbol details tool
    ├── status.go        # Index status tool
    └── affected.go      # Affected test files tool
```

## License

MIT

# codegraph-go

A lightweight **MCP server** that provides code intelligence tools for AI coding agents.  
Built as a thin Go layer over [ripgrep](https://github.com/BurntSushi/ripgrep) — no AST analysis, no heavy indexes, just fast file-level operations.

Designed as a drop-in for the official [`colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph) MCP surface (8 tools as of v0.2.0). Agents like [Reasonix](https://github.com/dorokuma/DeepSeek-Reasonix) register it under the name `codegraph`.

---

## Features

| Tool | What it does |
|------|-------------|
| `search` | Text/regex search across the workspace (ripgrep, respects `.gitignore`) |
| `files` | Glob file listing with `**` support (ripgrep `--files`) |
| `context` | Read a window of lines around a file:line position |
| `explore` | Project overview — top-level dirs, READMEs, manifests |
| `callees` | Extract function calls from a symbol's definition body |
| `callers` | Find all references to a symbol across the workspace |
| `trace` | Grep with optional ±5 lines of surrounding context |
| `impact` | Per-file match-count summary for a symbol |

**v0.2.0** removes the `status` tool (full-tree file/LOC walk). Use `explore` or `search` to verify workspace and MCP health.

### Design decisions

- **No AST.** All tools work via ripgrep + `os.ReadFile`. This keeps the binary small (~8 MB), startup instant, and the index nonexistent. It handles multi-language codebases with zero configuration.
- **Token-aware.** All text output is truncated at 50 KB — agents (and their context windows) stay healthy.
- **Path-safe.** Every tool resolves user-supplied paths against a workspace root; path traversal is rejected with a clear error.
- **`.gitignore`-aware.** Tools that scan files use ripgrep, which respects `.gitignore` by default.

---

## Installation

```bash
# Prerequisites
which rg        # ripgrep must be on PATH
go version      # Go 1.25+ (see go.mod)

# Build from source
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

The server speaks the [MCP protocol](https://modelcontextprotocol.io) over stdio.  
Connect any MCP client — or add it as a Reasonix / Grok MCP server.

### Reasonix plugin

Add to `reasonix.toml`:

```toml
[[plugins]]
name    = "codegraph"
command = "/usr/local/bin/codegraph-go"
```

The agent discovers all 8 tools on the next startup.

### Grok

```toml
[mcp_servers.codegraph]
command = "/path/to/codegraph-go"
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-workdir` | current directory | Workspace root; all paths are resolved relative to this |

---

## Tools

### `search`

```json
{
  "pattern": "func main",
  "path": "src/",
  "glob": "*.go",
  "max_results": 50,
  "ignore_case": false
}
```

Returns matching lines with file:line prefix. All parameters except `pattern` are optional.

### `files`

```json
{
  "pattern": "src/**/*.go",
  "max": 100
}
```

Lists files matching the glob, relative to workspace root. Respects `.gitignore`.

### `context`

```json
{
  "file": "main.go",
  "line": 42,
  "before": 15,
  "after": 15
}
```

Reads a window of lines centered on `line`. The target line is marked with `>>`.  
Path traversal is rejected.

### `callees`

```json
{
  "name": "handleMessage",
  "max_results": 50
}
```

Finds the definition of `name`, brace-matches its function body (up to 300 lines), and extracts every `symbol()`
call — filtering out keywords, control flow statements, and the symbol itself. Handles strings, comments,
and backtick literals correctly for most C-family languages.

### `callers`

```json
{
  "name": "handleMessage",
  "max_results": 100
}
```

Word-boundary grep for `name` across the workspace. Returns every line that references it.

### `trace`

```json
{
  "name": "handleMessage",
  "depth": 0
}
```

Grep with `-w`; `depth=1` adds ripgrep's `-C 5` (5 lines of surrounding context).

### `impact`

```json
{
  "name": "handleMessage",
  "max_results": 100
}
```

Per-file match count (`rg -c -w`). Answers "which files reference this symbol and how often?"

### `explore`

```json
{ "max": 30 }
```

Lists top-level non-dot entries in the workspace root, then finds README files.

---

## Limitations

- **No AST analysis.** `callees` uses brace-matching + regex heuristics, not a parser. It works well for Go, Rust, C, JavaScript, Python, and similar brace-delimited languages, but will miss dynamic calls and may include false positives from string literals that look like function calls.
- **Project-local only.** Unlike the official codegraph, there is no persistent symbol index, no cross-project queries, and no incremental indexing.
- **Language-agnostic heuristics.** The definition regex targets `func`, `function`, `def`, `defn`, `fn`, `class` — adequate for most common languages but not exhaustive.

---

## License

MIT
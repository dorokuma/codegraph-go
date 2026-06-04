# codegraph-go

A lightweight **MCP server** that provides code intelligence tools for AI coding agents.  
Built as a thin Go layer over [ripgrep](https://github.com/BurntSushi/ripgrep) — no AST analysis, no heavy indexes, just fast file-level operations.

Designed as a drop-in for the official [`colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph) MCP surface, it registers the same 9 tools so agents like [Reasonix](https://github.com/dorokuma/DeepSeek-Reasonix) can use them without configuration changes.

---

## Features

| Tool | What it does |
|------|-------------|
| `codegraph_search` | Text/regex search across the workspace (ripgrep, respects `.gitignore`) |
| `codegraph_files` | Glob file listing with `**` support (ripgrep `--files`) |
| `codegraph_context` | Read a window of lines around a file:line position |
| `codegraph_explore` | Project overview — top-level dirs, READMEs, manifests |
| `codegraph_status` | File count and LOC summary |
| `codegraph_callees` | Extract function calls from a symbol's definition body |
| `codegraph_callers` | Find all references to a symbol across the workspace |
| `codegraph_trace` | Grep with optional ±5 lines of surrounding context |
| `codegraph_impact` | Per-file match-count summary for a symbol |

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
go version      # Go 1.25+ (or build with your version)

# Build from source
cd /opt/codegraph-go
go build -o /usr/local/bin/codegraph-go .

# Or just grab the binary if you have one
cp codegraph-go /usr/local/bin/
```

## Usage

### Standalone (stdio)

```bash
codegraph-go -workdir /path/to/project
```

The server speaks the [MCP protocol](https://modelcontextprotocol.io) over stdio.  
Connect any MCP client — or add it as a Reasonix plugin.

### Reasonix plugin

Add to `reasonix.toml`:

```toml
[[plugins]]
name    = "codegraph"
command = "/usr/local/bin/codegraph-go"
```

The agent will discover all 9 tools on the next startup.

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-workdir` | current directory | Workspace root; all paths are resolved relative to this |

---

## Tools

### `codegraph_search`

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

### `codegraph_files`

```json
{
  "pattern": "src/**/*.go",
  "max": 100
}
```

Lists files matching the glob, relative to workspace root. Respects `.gitignore`.

### `codegraph_context`

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

### `codegraph_callees`

```json
{
  "name": "handleMessage",
  "max_results": 50
}
```

Finds the definition of `name`, brace-matches its function body (up to 300 lines), and extracts every `symbol()`
call — filtering out keywords, control flow statements, and the symbol itself. Handles strings, comments,
and backtick literals correctly for most C-family languages.

### `codegraph_callers`

```json
{
  "name": "handleMessage",
  "max_results": 100
}
```

Word-boundary grep for `name` across the workspace. Returns every line that references it.

### `codegraph_trace`

```json
{
  "name": "handleMessage",
  "depth": 0
}
```

Grep with `-w`; `depth=1` adds ripgrep's `-C 5` (5 lines of surrounding context).

### `codegraph_impact`

```json
{
  "name": "handleMessage",
  "max_results": 100
}
```

Per-file match count (`rg -c -w`). Answers "which files reference this symbol and how often?"

### `codegraph_explore`

```json
{ "max": 30 }
```

Lists top-level non-dot entries in the workspace root, then finds README files.

### `codegraph_status`

```json
{}
```

Returns JSON with file count, LOC, version, and workspace root.  
Skips binary files (by extension) and files larger than 10 MB.

---

## Limitations

- **No AST analysis.** `codegraph_callees` uses brace-matching + regex heuristics, not a parser. It works well for Go, Rust, C, JavaScript, Python, and similar brace-delimited languages, but will miss dynamic calls and may include false positives from string literals that look like function calls.
- **Project-local only.** Unlike the official codegraph, there is no persistent symbol index, no cross-project queries, and no incremental indexing.
- **Language-agnostic heuristics.** The definition regex targets `func`, `function`, `def`, `defn`, `fn`, `class` — adequate for most common languages but not exhaustive.

---

## License

MIT

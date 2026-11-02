# codegraph-go

A Go MCP server for code intelligence with SQLite indexing and auto-sync.

Based on [colbymchenry/codegraph](https://github.com/colbymchenry/codegraph) — official 8 MCP tools + `affected` extension.

Current version: **0.9.12**. Index logic version **19**.

Pipeline: extract → park cross-file refs → `ResolveAll` → scrub pure-noise failed refs → `SynthesizeAll` (callback / React / JSX / bridge / C fn-pointer / GoFrame). Nodes carry qualified_name / signature / visibility / is_exported / return_type. Vue/Svelte/Astro SFCs get a file component + script/frontmatter + template component refs. IndexAll uses a file-level worker pool (`CODEGRAPH_INDEX_WORKERS`). Optional shared daemon (one writer per project, N thin stdio proxies). Extractor logic bumps raise the index schema revision: the daemon's first start after an upgrade detects the mismatch and automatically wipes + fully rebuilds the index (no manual step; see [Indexing](#indexing)).

## Features

Alignment: steps **1–9** done incl. 7.5 (logic **19**). Not full feature-parity (next: step 10 eval).

- **1 MCP tool `codegraph` (action router):** `action=explore` (PRIMARY), `node`, `search`, `callers`/`callees`/`impact`, `files`, `status`, `affected`/`communities`, `store_fact`/`search_facts`. Same capabilities as 0.7 multi-tool surface; one schema for lower prompt cost.
- **node dual mode:** `file` alone = Read-like numbered source + dependents; `name` = body + trail; overloads return every body in one call
- **projectPath on every tool:** walk up to nearest `.codegraph/` and query that project’s index (no cross-project DB bleed)
- **Graph-first queries:** callers / callees / impact walk the SQLite call graph (rg only as labeled fallback); optional `file` pins overloads
- **Extract → unresolved_refs → ResolveAll → scrub → SynthesizeAll:** cross-file calls parked then linked; pure framework noise without a project symbol is scrubbed
- **Path aliases / monorepo:** tsconfig/jsconfig `paths`, `@/` fallbacks, package/pnpm workspaces, go.mod module+replace, Cargo workspace members
- **Dynamic-dispatch edges:** callback / EventEmitter / React-render / JSX-child / bridge / C fn-pointer / GoFrame route (heuristic + synthesizedBy)
- **Smart explore:** Flow path + source for a bag of symbols; size-tier output budget
- **SQLite indexing:** symbols, edges, files, unresolved_refs in `.codegraph/codegraph.db`
- **FTS5 full-text search:** plain identifiers in `search` hit FTS first (no separate search_fts tool)
- **Tree-sitter AST parsing:** Go / TypeScript / JavaScript / Python / C / C++ / Java / Kotlin / Rust / Ruby / PHP / C# / Scala / Swift / Lua with qualified_name, signature, is_exported, visibility, return_type (syntax-keyword call filter only)
- **SFC support:** Vue / Svelte / Astro — file component, multi-script/frontmatter, opening-tag template refs (HTML natives skipped, kebab→Pascal, `@click`/`v-on`)
- **Parallel index:** IndexAll worker pool (`CODEGRAPH_INDEX_WORKERS`, default min(8, cores-1)); DB writes serialized
- **Noise rules:** `ShouldParkRef` keeps real symbols even if named like `emit`; scrub after resolve
- **Regex fallback:** Dart, Luau, R, Objective-C, Liquid, Pascal/Delphi (regex is also the fallback when tree-sitter parsing fails, incl. Rust use/fn/impl + pub/signature + cargo map)
- **Framework route detection:** Gin, chi, gorilla/mux, GoFrame, Express, NestJS, React Router, Flask, FastAPI, Django, Laravel, Rails, Spring, ASP.NET, Axum, actix, Rocket, Vapor, Play
- **Cross-language bridging:** CGo (Go↔C), Python ctypes/cffi/Cython, React Native/Expo, Swift↔ObjC
- **Home-mode indexing:** workdir=`$HOME` only enters project-like top-level dirs
- **Shared daemon (optional):** one process per project root owns SQLite + watcher; MCP hosts attach via Unix socket proxy. `CODEGRAPH_NO_DAEMON=1` keeps the old embedded mode. Idle exit default 300s (`CODEGRAPH_DAEMON_IDLE_TIMEOUT_MS`); a daemon whose workdir disappeared (a deleted `.wt` worktree) exits on its own at the next liveness tick once no session is active, instead of holding the lock until the idle timeout.
- **content_hash incremental:** SHA-256 of file bytes; unchanged content skips re-extract even if mtime moved
- **Git-assist sync:** after cold index, `git status` picks up edits missed while nothing was watching (no hooks installed)
- **Portable index paths:** files/nodes stored relative to workdir (schema 17+); safe to move/copy the tree after reindex
- **Auto-sync:** file watcher with 2-second debounce on every configured workdir; new directories are watched recursively
- **Staleness warning:** warns when referenced files are pending sync
- **Index skip rules:** indexing applies built-in skip rules (internal skip.go) and does not parse `.gitignore`; search tools run ripgrep, which respects ignore files by default and only sweep ignored files when `no_ignore=true` is passed
- **Agent fact storage:** `store_fact` / `search_facts` tools let agents attach cross-session findings, decisions, and notes to code symbols; facts survive index rebuilds


## Progress

Aligned steps **1–9** (including optional **7.5** C fn-pointer + GoFrame synthesis). Remaining: **step 10** evaluation/docs wrap-up.

| Item | Value |
|------|-------|
| Display version | 0.9.12 |
| Index logic | 19 |
| Feature parity | **not claimed** (step 10 open) |

## Installation

```bash
# Prerequisites
which rg        # ripgrep must be on PATH
go version      # Go 1.25+

# Build
cd codegraph-go
go build -o codegraph-go ./cmd/codegraph-go

# Install
# Recommended: run ./deploy.sh — it installs to $HOME/.local/bin/codegraph-go
# as 0555 (read-only executable) plus an immutable bit (chattr +i) when running
# as root on a filesystem that supports it, so a stray `cat > binary` cannot
# quietly corrupt the deployed build. Set CODEGRAPH_SKIP_IMMUTABLE_LOCK=1 to
# install without the immutable bit (used by the script's own tests).
cp codegraph-go ~/.local/bin/codegraph-go
```

## Pi integration

A Pi-specific TypeScript adapter is maintained in [`integrations/pi/`](integrations/pi/README.md). The Go binary remains a generic stdio MCP server; the adapter owns Pi lifecycle, tool registration, dynamic index-root context, and output budgets.

## Usage

### Standalone (stdio)

```bash
codegraph-go -workdir /path/to/project
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-workdir` | current directory | Workspace root |
| `-config` | auto-discovered | Path to the YAML config file (see below) |
| `-no-sync` | false | Disable auto-sync file watcher |
| `-warm-daemon` | false | Bring the shared daemon up through the ordinary client path (`EnsureAndDial` → `SpawnDetached`, CPU-quota wrapper included), hang up and exit. Used by `deploy.sh`, which no longer starts a daemon of its own — that private entry point used to bypass `CPUQuota` and every other layer of the Go spawn path. |

### Config file

Optional YAML config (`-config` > `$CODEGRAPH_CONFIG` > `./codegraph-config.yaml` > `~/.config/codegraph/config.yaml`):

```yaml
workdirs: [/path/to/project]     # authority roots for workdir validation
exclude_dirs:                  # extra dirs to skip while indexing/watching
  - "*-backup*"                 # bare name or glob matched on dir basename
  - experiments/scratch         # workdir-relative slash path
  - "./ref"                     # top-level anchor: only <workdir>/ref and its
                               # subtree — a deep proj/internal/ref is NOT excluded
nice: 10                       # process nice level, clamped to [0,19] (0 = untouched;
                               # Linux: bounded multi-pass sweep over /proc/self/task,
                               # best-effort — see the resource-limits note)
max_procs: 4                   # GOMAXPROCS cap, clamped to [0,64] (0 = unlimited);
                               # an explicit $GOMAXPROCS env always wins
index_workers: 3               # parallel extract workers
                               # ($CODEGRAPH_INDEX_WORKERS env > this > default min(8, cores-1))
cpu_quota_percent: 150         # launch the daemon under a systemd-run scope with
                               # CPUQuota=150%, clamped to [0,6400] (0 = off; falls
                               # back to a plain spawn on non-systemd hosts or when
                               # the scope launch fails — never blocks daemon start)
```

Env overrides: `CODEGRAPH_EXCLUDE_DIRS` (comma-separated, appends),
`CODEGRAPH_NICE`, `CODEGRAPH_MAX_PROCS`, `CODEGRAPH_CPU_QUOTA_PERCENT`.
The built-in skip list already covers dependency caches, build outputs and
the `.wt` git-worktree container; the workdir root itself is never skipped
even when it sits inside an excluded path (e.g. a single worktree as
workdir). The same built-in skip dirs and `skipPathFragments` cache trees
(node_modules, `.local/lib`, `go/pkg`, `.cargo/registry`, …) are also
excluded from rg-based `search`/`files`/`callers`/`callees`/`impact`, so
the rg backstop and the indexer never disagree about what is in scope.

`exclude_dirs` rules are read with `filepath.Match` semantics plus an
exact-name comparison (the indexer's engine). Before they reach ripgrep
they go through `extraction.RgExcludeGlobs`, which emits up to two globs
per rule — a wildcard translation and a literal one — mirroring the
walker's "exact-name OR filepath.Match" dual semantics: a valid Go
pattern whose own literal name it does not match (`\{foo`, `[ab]`) is
still excluded by name on both sides. The translation keeps the shape
(`./sub` stays anchored to the workdir root) and rewrites what the two
glob engines disagree on: `\`, `{` and `}` become the classes `[\\]`,
`[{]` and `[}]` (rg's brace expansion runs before escape resolution and
in-class `\` is literal), and `[...]` classes are re-emitted through
POSIX class normalisation — reversed ranges (`[z-a]`, silently empty for
Go but exit 2 for rg) are suppressed, `[^z-a]` collapses to `?`, and
in-class `]`/`!`/`^`/`-` are placed where rg reads them literally.
Bare-name rules (no `/` in the rule) get one more fix: the walker only
ever sees a basename, which can never be `/`, but rg's `[!x]` and any
range spanning `/` would otherwise match the separator and kill a
two-level dir like `cmd/codegraph-go` — so `/` is injected into
negated classes (`[!x]` → `[!x/]`, emitted `**/`-prefixed because the
glob now carries `/`) and positive ranges are split at the separator
(`[\--0]` → `[.0-]`). Path rules (`a/b`, `./sub`) keep `/` as a legal
class member — `filepath.Match("a[/]b","a/b")` is true and rg 14.1.1
honours it identically. A rule `filepath.Match` rejects (`data[`, a
trailing dangling escape like `a\`, or a `*`-suffixed malformed tail
like `foo*\`) is passed to rg fully escaped — `a\` becomes `a[\\]`,
`foo*\` becomes `foo\*[\\]` — which rg parses and which excludes
exactly the literal directory. Validity is checked by `isValidGoPattern`,
which validates every `*`-separated chunk instead of relying on
`filepath.Match(pat, "")` (it short-circuits at the first failing `*`
chunk and never sees a malformed tail). No rule can ever abort a
search (no exit 2) and no rule can panic the translator.

The glob conversion layer (character classes, brace expansion,
backslash escaping) is aligned against **ripgrep 14.1.1** (`globset`)
measured behaviour. After upgrading the local `ripgrep`, re-verify
compatibility with:

```
go test -p 2 ./internal/extraction ./internal/server -run TestRgExcludeArgsWeirdRulesAgreeWithWalker
```

### Shared daemon socket auth

The optional shared daemon serves MCP sessions over a Unix socket that is
always `chmod 0600` (bind fails otherwise) — the first line of defense. As
a second layer, e.g. against other processes of the same user, the daemon
accepts an optional shared secret via `CODEGRAPH_MCP_TOKEN`:

- Set the same value for both the daemon and every connecting client (the
  stdio proxy sends it in its client hello). A daemon spawned by a client
  inherits its environment; an already-running daemon must be restarted
  with the variable set.
- With the token set on the daemon, a session that never sends a client
  hello, or presents a non-matching token, is dropped before it can issue
  any tool call. Comparison is constant-time and the token is never logged.
- Default off: with the variable unset on the daemon, behavior is
  unchanged. When set, clients without the token are rejected — both
  sides must carry the same value.

## MCP Tool

**One tool:** `codegraph`. Required: `action`. Optional common fields: `path`, `projectPath`, `max` / `max_results`, `glob`, etc. (see tool schema).

| action | Purpose |
|--------|---------|
| `explore` | **PRIMARY.** Overview or `query=` bag of names → Flow + source (treat as already Read) |
| `node` | `file` alone = Read-like source + dependents; `name` = body + trail |
| `search` | Symbol FTS or ripgrep (`pattern`); literal by default — `regex=true` for regex, `no_ignore=true` to include ignored files |
| `callers` / `callees` / `impact` | Call graph; optional `file` pin |
| `files` | Glob listing |
| `status` | Index health |
| `affected` | Tests affected by changed `files` |
| `communities` | Module/community structure |
| `store_fact` / `search_facts` | Cross-session facts |

`projectPath` (absolute path inside a project) selects the nearest `.codegraph/` index.

### Hosts

| Host | Invoke |
|------|--------|
| Any MCP client | tool `codegraph` + `action` |
| Grok | `cg__codegraph` or `cg-eqi12__codegraph` + `action` (config keys `mcp_servers.cg` / `cg-eqi12`) |
| Pi | extension tool `codegraph` + `action` (see `integrations/pi/`) |

## Indexing

On first run, codegraph-go indexes the entire project. The index includes:
- **Nodes:** functions, methods, classes, structs, interfaces, variables, constants
- **Edges:** calls, imports, extends, implements

The file watcher automatically re-indexes changed files within 2 seconds.

### Rebuilds

Extractor logic changes (new node kinds, edge fixes, call-site corrections, …)
make old rows wrong or incomplete, so they raise the index schema revision.
On startup the daemon compares the on-disk revision against the binary's
constant: a mismatch wipes the symbol index and rebuilds it in full, with no
manual step. Upgrading to a release with new extraction semantics therefore
rebuilds automatically on the first start after the upgrade — the rebuild
runs inside the daemon while it holds the index lock, takes as long as the
initial index, and queries may return partial results while it runs
(`status` shows the pending rebuild).

Incremental re-indexing only applies to files added or changed *after* that
rebuild: the content_hash gate skips re-extraction of unchanged files, so an
index built by older logic is never "self-healed" by incremental sync — only
files whose content changes get re-extracted under the new semantics.

### Tree-sitter vs Regex

For Go, TypeScript, JavaScript, Python, C, C++, Java, Kotlin, Rust, Ruby, PHP, C#, Scala, Swift, and Lua, codegraph-go uses tree-sitter for accurate AST-based extraction. This provides:
- Better method detection (including struct methods with receivers)
- More accurate call graph (handles method calls, selector expressions)
- Proper handling of nested functions and closures

For other languages, regex-based extraction is used as a fallback.

### Framework Route Detection

codegraph-go detects web framework routing patterns and creates `route` nodes:

- **Go:** Gin, chi, gorilla/mux, GoFrame
- **JavaScript/TypeScript:** Express, NestJS, React Router
- **Python:** Flask, FastAPI, Django
- **PHP:** Laravel
- **Ruby:** Rails
- **Java:** Spring
- **C#:** ASP.NET
- **Rust:** Axum, actix, Rocket
- **Swift:** Vapor
- **Scala:** Play

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
├── cmd/codegraph-go/main.go    # Entry point
├── internal/db/
│   ├── schema.sql       # SQLite schema
│   ├── connection.go    # Database connection
│   ├── root.go          # FindNearestCodeGraphRoot
│   └── query.go         # nodes, edges, files, unresolved_refs
├── internal/extraction/
│   ├── common.go                # Language detection
│   ├── extractor_core.go        # Regex extractor dispatch + shared helpers
│   ├── extractor_brace.go       # Brace/indent body scanning
│   ├── extractor_c_braced.go    # Go + JS regex extraction
│   ├── extractor_generic.go     # Generic regex extraction (other languages)
│   ├── extractor_misc.go        # ObjC / Liquid / Lua / Pascal regex extraction
│   ├── extractor_python_rust.go # Python + Rust regex extraction (Rust use/fn)
│   ├── treesitter.go            # Tree-sitter AST (15 languages)
│   ├── frameworks.go            # Framework route detection
│   ├── bridge.go                # Cross-language bridging
│   └── orchestrator.go          # Index builder → ResolveAll
├── internal/resolution/
│   ├── resolver.go      # pending refs → edges
│   ├── name_matcher.go
│   ├── import_resolver.go
│   ├── path_aliases.go  # tsconfig/jsconfig paths
│   ├── go_module.go     # go.mod module + replace
│   ├── workspace_packages.go
│   └── cargo_workspace.go
├── internal/server/
│   ├── server.go        # MCP server handler
│   ├── tools.go         # Tool dispatcher
│   ├── project.go       # Project management
│   └── output.go        # Response formatting
├── internal/sync/
│   └── watcher.go       # File watcher with debounce
├── internal/tools/
│   ├── graph.go         # explore / callers / callees / impact
│   ├── node.go
│   ├── status.go
│   └── affected.go
├── testdata/parity/     # go, ts, py, alias, gomod, cargo, synth_*
```

## License

MIT

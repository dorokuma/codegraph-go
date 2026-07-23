# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

## [0.6.0] - 2026-07-23

### Added
- YAML config file support (`-config` flag > `$CODEGRAPH_CONFIG` env > `./codegraph-config.yaml` > `~/.config/codegraph/config.yaml`)
- Multi-workdir indexing: additional directories in `workdirs:` list are indexed into their own `.codegraph/codegraph.db`
- `detectProject` scans all workdirs for project markers, stores full paths
- `ToolExplore` / `ToolStatus` accept `workdirs` parameter; home-mode overview shows projects across all workdirs
- Cross-project DB cache: `resolveProject` uses `FindNearestCodeGraphRoot` to locate the correct DB, with LRU eviction and ref-counting
- `resolvePath` tries resolution across all workdirs

### Fixed
- All tool handlers now pass `args.Path` to `detectProject` — previously only name/query was checked, so `path=<project>` was silently ignored
- `cmd/codegraph-go/main.go`: added workdir deduplication after canonicalization
- `internal/config/config.go`: prepend dedup check now iterates all elements instead of only checking index 0

## [0.5.0] - 2026-07-22

### Added
- `tools/node.go`: `includeCode` defaults to `false` — symbol mode returns location + signature + call chain only, no source body
- `tools/graph.go` + `main.go`: `ExploreArgs` gains `SkipCode` field; when `skipCode=true`, code blocks are replaced with line-count summaries
- Extension `codegraph-go.ts`: auto-injects `includeCode=false` / `skipCode=true`; added `formatCleanText` to strip markdown formatting (aligns with Read tool style); system prompt and tool descriptions updated

### Fixed
- `extraction/orchestrator.go`: index worker goroutines now `defer recover()` to prevent panics from crashing the process
- `daemon/proxy.go`: `io.Copy` errors are now logged instead of silently swallowed
- `callees_fallback.go`: rg calls now have a 10-second timeout to prevent hangs

## [0.4.0] - 2026-06-XX

- **12 MCP tools:** search, search_fts, files, context, explore, callees, callers, trace, impact, node, status, affected.
- **SQLite index + FTS5:** symbols/edges/files in `.codegraph/codegraph.db`; `search_fts` for indexed symbol search.
- **FTS hardening:** escape free-text queries; backfill `nodes_fts` on upgrade from pre-FTS databases.
- **24 languages:** Go/TS/JS/Python tree-sitter; regex fallback for Rust, Java, C#, Ruby, PHP, C/C++, Swift, Kotlin, Scala, Dart, Lua/Luau, R, Objective-C, Svelte, Vue, Astro, Liquid, Pascal/Delphi.
- **17 framework route families:** Gin/chi/mux, Express, NestJS, Flask/FastAPI/Django, Laravel, Rails, Spring, ASP.NET, Axum/actix/Rocket, Vapor, Play.
- **Cross-language bridges:** CGo, Python C extensions, React Native/Expo, Swift ↔ ObjC.
- **Auto-sync:** fsnotify watcher with debounce; staleness warning when pending files exist.
- Background cold index so MCP initialize is never blocked; non-blocking stderr logger.
- Output caps and shared truncate helpers for agent token budgets.

## [0.3.1]

- Remove MCP tool `status` (full-workspace file/LOC walk). Use `explore` or `search` instead.
- Drop unused `sync.Mutex`, `encoding/json`, and dead helpers after status removal.

## [0.3.0]

- Remove redundant `codegraph_` prefix from tool names (breaking change for existing users).

## [0.2.4]

- Refactor toolSearch to use rg.Output() to avoid process kill/wait deadlocks on containers.

## [0.2.3]

- Resolve memory leak in toolStatus by skipping huge cache directories and streaming files.

## [0.2.2]

- Handle Python triple-quotes (''' """) in stripStringsAndComments.
- Add countLeadingSpaces helper.

## [0.2.1]

- Filter out comments/strings when checking open brace for function body detection.

## [0.2.0]

- Auto resolve directory pattern to recursive glob in toolFiles.

## [0.1.3]

- Fix duplicate function call suppression bug across different definitions.

## [0.1.2]

- Fix pseudo-definition search bug and mitigate search OOM risk.

## [0.1.1]

- Optimize status LOC scanning with cache, resolve trace truncation and readLines OOM.

## [0.1.0]

- Initial 9-tool ripgrep-based MCP surface matching colbymchenry/codegraph shape.

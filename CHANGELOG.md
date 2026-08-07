# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

## [0.8.7] - 2026-08-07

### Fixed
- Pi 适配器手动 `codegraph-start` 的状态机：`getClient` 优先返回已启动的
  client 再判会话决策，手动拉起成功后把本次决策写回 `decision`，后续工具
  调用直接复用已启动客户端，不再出现「已拉起仍报尚未初始化/未启用」的
  假失败；`codegraph-info` 在会话已有决议时以其为准，展示与实际一致。
- Pi 适配器 config 解析支持 flow-style workdirs：`workdirs: [/root, /opt]`
  同行内联列表与 block 列表同样解析（可混排，支持引号与行尾注释），不再
  回落 $HOME 与 Go 侧 yaml.Unmarshal 分叉。
- Pi 适配器启动阶段不再二次决议：新增 `startClientAt` 直接使用已决议
  workdir（决议只发生一次：session_start / 手动 start），启动时不再
  consult `CODEGRAPH_GO_WORKDIR`，避免已过 allowlist 的决议 workdir 被
  env 改道且改道结果不再过授权校验。
- `deploy.sh` 杀进程前做完整谓词复检：新增 `daemon_matches`（argv0 基名
  ∈ {codegraph, codegraph-go} + `-workdir` 精确匹配 + environ 含
  `CODEGRAPH_DAEMON_INTERNAL=1`），对每个待杀 pid（含 pidfile 读出的）
  在 SIGTERM 前与 SIGKILL 升级前各复检一次，杜绝 PID 复用误杀窗口；
  `scan_daemon_pids` 复用同一函数，消除重复逻辑。

### Changed
- Display / daemon wire version **0.8.7**。

## [0.8.6] - 2026-08-07

### Fixed
- Pi 适配器改为按需拉起：`session_start` 不再全局 spawn codegraph 客户端，只在
  会话开头做工作目录决策与工具注册；客户端在首次工具调用时才真正启动，进程
  异常退出不再定时重启（下一次调用自动拉起）。
- Pi 适配器在会话开头按 config 授权根判定可用性：`makeWorkdirDecision` 在
  `resolveWorkdir` 之后对最终 workdir 做路径段级授权范围校验（与二进制
  `ValidateWorkdirs` 同语义，config 文件优先级一致，无 config 回落 `$HOME`），
  越界会话不注册工具、注入「本会话 CodeGraph 未启用」并拒绝 execute。

### Changed
- Display / daemon wire version **0.8.6**。

## [0.8.5] - 2026-08-07

### Fixed
- Config `workdirs` are now enforced as the authority allowlist: a workdir
  outside the declared roots (or not a descendant of one) is refused before
  any mode is entered — client, daemon and direct alike — with canonical
  (abs + symlink-resolved) comparison so symlink escapes are rejected. With
  no usable config the allowlist defaults to `$HOME`, and when `$HOME`
  itself cannot be resolved validation fails closed (every workdir is
  refused). `init <root>` enforces the same guard so it is not a side door.

### Changed
- Display / daemon wire version **0.8.5**.

## [0.8.4] - 2026-08-06

### Fixed
- Invisible flock holder self-heal: when every candidate socket is
  unreachable yet the DB lock is still held by a daemon whose pidfile/socket
  were removed (deploy race), `EnsureAndDial` locates the holder via `/proc`
  (codegraph argv + `-workdir` match + `CODEGRAPH_DAEMON_INTERNAL=1`) and
  replaces it instead of looping forever. The holder's identity is
  re-verified against live `/proc` immediately before every signal (SIGTERM
  and the SIGKILL escalation) to guard against PID reuse between scan and
  signal; a `/proc` scan failure is treated as "no holder found" instead of
  a fatal error; and the held-lock probe is a lightweight `flock(2)` check
  rather than a full database open.
- `deploy.sh` no longer removes `daemon.pid`/`daemon.sock` while a daemon
  might still hold the DB lock: it kills every daemon-mode process for the
  workdir, waits for the flock to be released, and only then removes
  artifacts; pre-warm readiness now requires pidfile + socket + held lock +
  live process.
- The "database in use" error no longer reports "held by pid <self>" when
  the pidfile names the reporting process itself (the daemon's own
  freshly-written pidfile during an invisible-holder wedge).

### Changed
- Display / daemon wire version **0.8.4**.

## [0.8.3] - 2026-08-06

### Fixed
- rg fallback error handling: exit-1 (no matches) is now distinct from a
  missing ripgrep binary or a timed-out search, so failures no longer surface
  as "no results".
- Symlink escape guard: the index skips symlinked files, and `callees` reads
  only paths resolved inside the project root (path jail).
- `ListUnresolvedRefsByFiles` queries are chunked by 400 files to stay within
  the SQLite variable limit.
- Index worker panics are counted as extraction errors and no longer mark the
  schema revision as rebuilt on interrupted or failed passes.
- A failed extraction keeps the previously indexed symbols and skips writing
  the new content hash, so the next pass automatically retries.
- Partial failures are counted and surfaced as a startup warning instead of a
  silent pass.
- `DefReCache` is a bounded cache (no unbounded memory growth).
- `affected` and root-directory queries honor their LIMIT at the boundary.
- Daemon startup failure releases the pidfile lock, so no zombie lock holder
  blocks the next start.
- Proxy hello failure exits non-zero instead of hanging in direct mode.
- Pi adapter keeps stderr buffered for diagnostics.
- README wording fix for `.gitignore`.

### Changed
- Display / daemon wire version **0.8.3**.

## [0.8.2] - 2026-08-06

### Fixed
- Single-writer database lock: the daemon takes an exclusive process lock on the
  index before opening it, cleans up a stale daemon (PID identity-validated)
  before re-acquiring, and reports who holds the lock when it cannot — two
  writers can no longer fight over the same index.
- Rebuild safety: `NeedsRebuild` no longer triggers a full index wipe on
  arbitrary read errors; a failed extraction keeps the file's previously indexed
  symbols, single-file reindexing is atomic, and index errors are surfaced
  instead of silently swallowed.
- Call-graph edges: multiple call sites between the same symbol pair now produce
  distinct edges (unique key includes line/column) via a crash-recovery
  migration, and synthesized edges are replaced atomically in a single
  transaction. Schema revision 17 → 18.
- Incremental sync hardening: re-extract is gated on a content hash with
  millisecond-precision mtime comparison; git-assist clears the index for
  deleted/renamed files; watcher overflow triggers a full rescan and
  re-registers directory watches; unresolved-reference scans are pushed down
  into SQL and tolerate empty tails.
- Daemon lifecycle: graceful exit when the parent proxy exits (PPID watch),
  PID-reuse guard so only the true stale daemon is killed, idempotent `Close`,
  and the safelog no longer panics when written after close.
- Safety nets: the path jail resolves symlinks, `projectPath` workdirs are
  allowlisted, `affected` depth is clamped, and `callees` falls back to the
  definition location when there are no callers.

### Changed
- Display / daemon wire version **0.8.2**; index schema revision **18**.
- Go directive bumped to 1.25.12 with toolchain go1.26.5 (stdlib CVE fixes);
  removed a 43 MB binary that was accidentally committed.

## [0.8.1] - 2026-08-06

### Fixed
- Pi adapter action dispatch: `withToolDefaults` now switches on `args.action`
  instead of the legacy tool name (the single `codegraph` tool never matched the
  old cases), so `max`/`max_results` clamping and defaults, `explore.skipCode`,
  `node.includeCode` and `affected.depth` defaults are effective again.
- Pi adapter captures server `instructions` from the MCP initialize handshake
  and injects them into the agent system prompt (`## CodeGraph server
  instructions`), ahead of the adapter's own `## CodeGraph Tools` section.

### Changed
- Display / daemon wire version **0.8.1**.
- gofmt comment alignment in `internal/db/query.go` and `internal/tools/community.go`
  (formatting only, no logic change).

## [0.8.0] - 2026-08-05

### Changed
- **Breaking:** MCP surface is a **single tool** `codegraph` with required `action`
  (`explore`|`search`|`files`|`node`|`callers`|`callees`|`impact`|`status`|`affected`|
  `communities`|`store_fact`|`search_facts`). Former top-level tools (`explore`,
  `search`, …) are no longer registered — same handlers, one schema (less model
  noise / tokens). Any MCP host (Grok, Pi, others) uses this entry directly.
- Server instructions and Pi adapter updated: Pi calls MCP tool `codegraph` with
  the full payload (including `action`); no per-action MCP name fan-out.
- Display / daemon wire version **0.8.0**.

### Migration
- Replace `tools/call` name=`explore` with name=`codegraph` + `action`=`explore`
  (same for other former tool names).
- Grok: `cg__codegraph` / `cg-eqi12__codegraph` + `action` (server keys `cg` / `cg-eqi12`).
- Pi: keep tool name `codegraph`; install adapter from `integrations/pi/` and
  binary ≥ 0.8.0 together.

## [0.7.1] - 2026-07-26

### Added
- `communities` / `store_fact` / `search_facts` now accept a `path` arg as a home-mode project selector (directory name under a broad workdir), retargeted via `detectProject` — same convention as `search`/`callers`/`explore`. Fixes pi-extension clients that pass `path` instead of `projectPath`.

## [0.7.0] - 2026-07-26

### Added
- `communities` MCP tool: Louvain community detection on the project call graph to reveal module/component boundaries for global architecture questions ("how is this project organized?", "what are the main modules?"). Uses gonum's `graph/community.Modularize` with fixed seed for deterministic results, projects directed edges as undirected with provenance-based weights (exact=1.0, import=0.8, proximity=0.3, heuristic=0.1), excludes `contains` edges, and caps output by community size.
- Agent fact storage: `facts` table in SQLite + `store_fact` / `search_facts` MCP tools.
  - `store_fact`: write agent findings attached to code symbols; SHA-256 dedup; supersede chain support; returns same-target facts for contradiction detection.
  - `search_facts`: query by content substring, file, symbol, or status.
  - Facts survive `WipeIndex` (index rebuilds don't delete them).
  - Absolute paths are normalized to workdir-relative before storing.

## [0.6.2] - 2026-07-25

### Added
- A versioned Pi-specific adapter under `integrations/pi/` with installation and configuration documentation.
- GitHub Actions CI for tests, vetting, and binary builds.

### Changed
- Pi runtime context now injects only dynamic index information; fixed tool workflow guidance remains in tool metadata and skills.
- Display and daemon wire version updated to 0.6.2.

## [0.6.1] - 2026-07-25

### Changed
- Index schema **17**: store workdir-relative paths for files/nodes/edges (portable across machines/checkouts; forces full rebuild)
- Display version **0.6.1**

### Fixed
- `affected`: same-package test discovery works with real indexes (relative storage keys + absolute BFS normalization)
- `affected` / MCP: reject `stdin=true` at the MCP handler; CLI/offline may still use stdin in `ToolAffected`
- Multi-workdir: secondary roots keep an open DB + file watcher (no longer cold-index-and-close only)
- Multi-workdir: tool `projectPath` reuses the secondary watcher DB (no second Open / dual writer)
- `files`: surface real `rg` failures instead of always reporting "no files matched"
- Daemon spawn inherits parent `-config` and `-no-sync`
- `CountFilesUnder`: escape LIKE wildcards; treat empty/"." as whole-index count
- SQLite DSN: percent-escape path characters (`#`, `?`, spaces, `&`)
- Import edges prefer `module` nodes over same-named functions
- Config: log read/parse errors instead of silent ignore
- README version/schema drift vs code
- Restored `ResolveBestTarget` / `TruncateBody` unit tests (accidentally dropped during path refactor)

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

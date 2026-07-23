# Changelog

版本规则：[语义化版本](https://semver.org/) (MAJOR.MINOR.PATCH)
- MAJOR：不兼容的 API 破坏
- MINOR：新功能，向后兼容
- PATCH：bug 修复，向后兼容

## 0.6.0 (multi-workdir support + query layer fix)

### MINOR（新功能）
- YAML 配置文件支持（`-config` flag > `$CODEGRAPH_CONFIG` env > `./codegraph-config.yaml` > `~/.config/codegraph/config.yaml`）
- 多 workdir 索引：`workdirs:` 列表中的额外目录会被索引到各自独立的 `.codegraph/codegraph.db`
- `detectProject` 跨所有 workdir 扫描项目，存全路径映射
- `ToolExplore` / `ToolStatus` 接受 `workdirs` 参数，home-mode 展示所有 workdir 下的项目
- 跨项目 DB 缓存：`resolveProject` 通过 `FindNearestCodeGraphRoot` 自动找到正确 DB，LRU 缓存 + 引用计数
- `resolvePath` 跨 workdir 尝试解析

### PATCH（bug 修复）
- 所有 tool handler 的 `detectProject` 调用追加 `args.Path`（之前只传 name/query，导致 `path=<项目名>` 不被识别）
- `cmd/codegraph-go/main.go` 加 workdir 去重（对标 ctxmode）
- `internal/config/config.go` prepend 判断从只看首元素改为遍历全列表

## 0.5.0 (audit fixes + concise output)

### MINOR（新功能）
- `tools/node.go`：`includeCode` 默认值从 true 改为 false。符号模式只返回位置+签名+调用链，不返回源码。
- `tools/graph.go` + `main.go`：`ExploreArgs` 新增 `SkipCode` 字段。`explore` 工具 `skipCode=true` 时代码块替换为行数摘要。
- 插件 `codegraph-go.ts`：自动注入 `includeCode=false` / `skipCode=true`；新增 `formatCleanText` 统一剥离 markdown 格式（对齐 Read 工具风格）；系统提示与工具描述同步更新。

### PATCH（bug 修复）
- `extraction/orchestrator.go`：索引 worker goroutine 加 `defer recover()` 防 panic 炸进程。
- `daemon/proxy.go`：`io.Copy` 错误记日志（不再静默吞掉）。
- `callees_fallback.go`：rg 调用加 10 秒超时防卡死。

测试 **271 passed**。logic **15**（未变）。插件 **0.4.0**。

## 0.4.0 (feature parity with upstream CodeGraph)

- **12 MCP tools:** search, search_fts, files, context, explore, callees, callers, trace, impact, node, status, affected.
- **SQLite index + FTS5:** symbols/edges/files in `.codegraph/codegraph.db`; `search_fts` for indexed symbol search.
- **FTS hardening:** escape free-text queries; backfill `nodes_fts` on upgrade from pre-FTS databases.
- **24 languages:** Go/TS/JS/Python tree-sitter; regex fallback for Rust, Java, C#, Ruby, PHP, C/C++, Swift, Kotlin, Scala, Dart, Lua/Luau, R, Objective-C, Svelte, Vue, Astro, Liquid, Pascal/Delphi.
- **17 framework route families:** Gin/chi/mux, Express, NestJS, Flask/FastAPI/Django, Laravel, Rails, Spring, ASP.NET, Axum/actix/Rocket, Vapor, Play.
- **Cross-language bridges:** CGo, Python C extensions, React Native/Expo, Swift ↔ ObjC.
- **Auto-sync:** fsnotify watcher with debounce; staleness warning when pending files exist.
- Background cold index so MCP initialize is never blocked; non-blocking stderr logger.
- Output caps and shared truncate helpers for agent token budgets.

## 0.3.1 (remove status tool)

- Remove MCP tool `status` (full-workspace file/LOC walk). Use `explore` or `search` instead.
- Drop unused `sync.Mutex`, `encoding/json`, and dead helpers after status removal.

## 0.3.0 (tool name change)

- 工具名去掉冗余 `codegraph_` 前缀（breaking change for existing users）。

## 0.2.4 (refactor toolSearch)

- Refactor toolSearch to use rg.Output() to avoid process kill/wait deadlocks on containers.

## 0.2.3 (fix memory leak)

- Resolve memory leak in toolStatus by skipping huge cache directories and streaming files.

## 0.2.2 (fix Python callees)

- Handle Python triple-quotes (''' """) in stripStringsAndComments.
- Add countLeadingSpaces helper.

## 0.2.1 (filter comments/strings)

- Filter out comments/strings when checking open brace for function body detection.

## 0.2.0 (auto resolve directory)

- Auto resolve directory pattern to recursive glob in toolFiles.

## 0.1.3 (fix duplicate function call)

- Fix duplicate function call suppression bug across different definitions.

## 0.1.2 (fix pseudo-definition search)

- Fix pseudo-definition search bug and mitigate search OOM risk.

## 0.1.1 (optimize status scanning)

- Optimize status LOC scanning with cache, resolve trace truncation and readLines OOM.

## 0.1.0 (initial release)

- Initial 9-tool ripgrep-based MCP surface matching colbymchenry/codegraph shape.

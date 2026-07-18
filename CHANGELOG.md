# Changelog

## 0.4.0

Feature parity release with upstream CodeGraph (minus CLI — intentionally out of scope).

- **12 MCP tools:** search, search_fts, files, context, explore, callees, callers, trace, impact, node, status, affected.
- **SQLite index + FTS5:** symbols/edges/files in `.codegraph/codegraph.db`; `search_fts` for indexed symbol search.
- **FTS hardening:** escape free-text queries; backfill `nodes_fts` on upgrade from pre-FTS databases.
- **24 languages:** Go/TS/JS/Python tree-sitter; regex fallback for Rust, Java, C#, Ruby, PHP, C/C++, Swift, Kotlin, Scala, Dart, Lua/Luau, R, Objective-C, Svelte, Vue, Astro, Liquid, Pascal/Delphi.
- **17 framework route families:** Gin/chi/mux, Express, NestJS, Flask/FastAPI/Django, Laravel, Rails, Spring, ASP.NET, Axum/actix/Rocket, Vapor, Play.
- **Cross-language bridges:** CGo, Python C extensions, React Native/Expo, Swift ↔ ObjC.
- **Auto-sync:** fsnotify watcher with debounce; staleness warning when pending files exist.
- Background cold index so MCP initialize is never blocked; non-blocking stderr logger.
- Output caps and shared truncate helpers for agent token budgets.

## 0.3.2

- Mid-point output caps: ~18k chars; search global 70 / per-file 12; files 100; callers/callees/impact 40; trace 70.
- `max_results` on search is a global match cap (per-file still capped separately).
- Shared truncate/limit helpers; truncation hints ask to narrow path/glob.

## 0.3.1

- Start MCP stdio immediately; full project index runs in the background (no cold-start hang).
- Non-blocking log writer so an unread stderr pipe cannot freeze the process.
- Skip dependency/toolchain cache dirs during walk (gradle, cargo, go/pkg/mod, node_modules, etc.).
- Add `codegraph-go init <root>` for hosts that pre-create `.codegraph/` (e.g. reasonix).
- Unify IndexAll / IndexAllWithProgress / IndexChanges through visitIndexable + indexIfNeeded.
- Keep the full MCP tool surface (search, files, context, explore, callees, callers, trace, impact, node, status, affected).

## 0.2.0

- Remove MCP tool `status` (full-workspace file/LOC walk). Use `explore` or `search` instead.
- Drop unused `sync.Mutex`, `encoding/json`, and dead helpers after status removal.
- MCP server version string `0.2.0`; documented 8-tool surface in README.

## 0.1.0

- Initial 9-tool ripgrep-based MCP surface matching colbymchenry/codegraph shape.
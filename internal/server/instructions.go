package server

// serverInstructions is sent in MCP initialize so agents learn the playbook once.
const serverInstructions = `# Codegraph-go — code intelligence over an indexed knowledge graph

Codegraph-go is a SQLite knowledge graph of symbols, edges, and files in the
workspace. One MCP tool — **codegraph** — with an **action** parameter (not a
dozen near-duplicate tools). Lower prompt noise, same capabilities.

## Primary: codegraph action=explore

- Almost any question ("how does X work", architecture, a bug, survey) →
  **codegraph** with action=explore and query= a natural-language question or
  bag of symbol/file names. ONE call returns Flow + symbols. Default
  skipCode=true omits bodies — set skipCode=false (or node includeCode=true)
  before treating the result as Read.
- Empty query = project overview. Flow from X to Y → query naming both ends.

## Other actions (same tool)

- **node** — file alone = Read-like numbered source + dependents; name = body
  trail (includeCode to see implementation).
- **search** — pattern is matched literally by default (simple identifiers hit FTS first); set regex=true for regex (ripgrep syntax); set no_ignore=true to also search .gitignore'd files.
- **callers** / **callees** / **impact** — call graph (pass file when overloaded).
- **files** — glob listing. **status** — index health.
- **affected** — tests after edits. **communities** — module structure.
- **store_fact** / **search_facts** — cross-session notes on symbols.

Common args: path (home-mode project name), projectPath (absolute), max, glob.

## Anti-patterns

- Don't re-verify codegraph with grep — the index is AST-based.
- Don't Read/Grep first for indexed code — explore with skipCode=false or node includeCode=true already return source.
- Don't reconstruct a flow by hand — name the endpoints in one explore.
- If a project isn't indexed, stop calling codegraph for THAT project this
  session and use built-in tools there; other projectPath targets still work.
- Index lags writes by ~1–2s via the file watcher.

## Host notes

- MCP tool name is always **codegraph** (action=…). Grok prefixes the server
  config key (e.g. cg__codegraph / cg-eqi12__codegraph). Pi may register the
  same name via an adapter that calls this MCP tool.
`

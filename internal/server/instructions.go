package server

// serverInstructions is sent in MCP initialize so agents learn the playbook once.
const serverInstructions = `# Codegraph-go — code intelligence over an indexed knowledge graph

Codegraph-go is a SQLite knowledge graph of symbols, edges, and files in the
workspace. Reach for it BEFORE and while editing — one call returns verbatim
source PLUS who calls it and what it affects. More accurate context, fewer
tokens and round-trips than a Read/Grep loop.

## Primary: explore

- Almost any question ("how does X work", architecture, a bug, survey) →
  **explore** with a natural-language question or bag of symbol/file names.
  ONE call returns source grouped by file + Flow path. Treat that source as
  already Read — do NOT re-open those files.
- Flow from X to Y → explore naming both ends (e.g. "mutateElement renderScene").

## Secondary: node / search / graph

- Read one file like Read → **node** with file only (line-numbered source +
  dependents). offset/limit work like Read; symbolsOnly for a cheap map.
- One named symbol (body + caller/callee trail) → **node** with name.
  Overloads return every body in one call; pass file/line to pin one.
- Find a name → **search** (simple identifiers hit the index FTS first).
- Who calls / what it calls / blast radius → **callers** / **callees** / **impact**
  (pass file when the name is overloaded). Prefer explore for multi-hop flows.
- Layout → **files**. Index health → **status**. After edits, which tests →
  **affected** (extension; not the main navigation path).

## Anti-patterns

- Don't re-verify codegraph with grep — the index is AST-based.
- Don't Read/Grep first for indexed code — explore/node already return source.
- Don't reconstruct a flow by hand — name the endpoints in one explore.
- If a tool says a project isn't indexed, stop calling codegraph for THAT
  project this session and use built-in tools there; other projectPath targets
  still work. Indexing is the user's decision (codegraph-go init).
- Index lags writes by ~1–2s via the file watcher.
`

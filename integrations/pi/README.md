# Pi integration

Thin stdio bridge from Pi to the **codegraph-go** MCP server.

- Starts/supervises the Go binary.
- Registers **one** Pi tool: `codegraph` (same name and `action=` surface as MCP v0.8+).
- Forwards the full argument object to MCP `tools/call` name=`codegraph` (no per-action fan-out).
- Applies Pi-side output budgets; optional dynamic index root in `before_agent_start`.

Any MCP client (Grok, Pi, others) should call the Go server’s single tool **`codegraph`** with **`action`**. The adapter is only required for Pi’s extension model.

## Install

```bash
go build -o codegraph-go ./cmd/codegraph-go
install -m 755 codegraph-go ~/.local/bin/codegraph-go
# Recommended: run ./deploy.sh from the repo root instead of manual install —
# it installs to $HOME/.local/bin/codegraph-go and avoids stray copies elsewhere.

install -m 644 integrations/pi/codegraph-go.ts ~/.pi/agent/extensions/codegraph-go.ts
```

The extension is synced by [`integrations/pi/install.sh`](install.sh) (also invoked by `./deploy.sh`): it copies `integrations/pi/codegraph-go.ts` to the destination and is md5-idempotent (an already-matching target is not rewritten, so its mtime is preserved). The destination defaults to `$HOME/.pi/agent/extensions/codegraph-go.ts`. Set **`PI_EXT_DEST`** to an absolute **file path** — not a directory — to override the destination (verification / multi-machine deploys). When `PI_EXT_DEST` is set, `HOME` is not consulted at all; `HOME` is only used to derive the default path when `PI_EXT_DEST` is unset.

Restart Pi or `/reload`. **Requires codegraph-go ≥ 0.9.0** (MCP only exposes
`codegraph`; 0.9.0 changed `search` defaults — literal matching and
`.gitignore`-aware by default, with `regex` / `no_ignore` opt-in flags — so
older binaries cannot express the adapter's search semantics). Use the
matching old adapter with 0.8.x binaries.

## Runtime configuration

- `CODEGRAPH_GO_BIN`: binary path.
- `CODEGRAPH_GO_START_TIMEOUT_MS` / `CODEGRAPH_GO_REQUEST_TIMEOUT_MS`.
- `CODEGRAPH_GO_OUTPUT_CHARS` / `CODEGRAPH_GO_OUTPUT_LINES`.
- `CODEGRAPH_GO_SEARCH_MAX` / `FILES` / `SYMBOL` / `EXPLORE` and corresponding `_HARD` caps.

## Grok vs Pi (same MCP)

| Host | How you invoke |
|------|----------------|
| Pi | Tool `codegraph` + `action` (this extension) |
| Grok | MCP `cg__codegraph` or `cg-eqi12__codegraph` + `action` |

Semantics are identical; only the host’s MCP server config key differs on Grok.

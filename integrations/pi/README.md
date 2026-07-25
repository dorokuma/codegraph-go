# Pi integration

This directory contains the Pi-specific adapter for the generic `codegraph-go` stdio MCP server.

- `codegraph-go.ts` starts and supervises the Go binary.
- It registers CodeGraph tools with `@earendil-works/pi-coding-agent`.
- It applies Pi-side parameter caps and output budgets.
- It injects only runtime-dependent index information, such as the active root and home-mode projects. Fixed usage guidance belongs in tool metadata and Pi skills instead of a per-turn system prompt.

The adapter is not required by other MCP clients.

## Install

Build and install the binary first:

```bash
go build -o codegraph-go ./cmd/codegraph-go
install -m 755 codegraph-go ~/.local/bin/codegraph-go
```

Then copy the adapter into Pi's user extension directory:

```bash
install -m 644 integrations/pi/codegraph-go.ts ~/.pi/agent/extensions/codegraph-go.ts
```

Restart Pi or run `/reload`.

## Runtime configuration

The adapter resolves `codegraph-go` from `PATH`. The principal optional variables are:

- `CODEGRAPH_GO_BIN`: explicit binary path.
- `CODEGRAPH_GO_START_TIMEOUT_MS` and `CODEGRAPH_GO_REQUEST_TIMEOUT_MS`: process and request timeouts.
- `CODEGRAPH_GO_OUTPUT_CHARS` and `CODEGRAPH_GO_OUTPUT_LINES`: Pi-side output caps.
- `CODEGRAPH_GO_SEARCH_MAX`, `CODEGRAPH_GO_FILES_MAX`, `CODEGRAPH_GO_SYMBOL_MAX`, and `CODEGRAPH_GO_EXPLORE_MAX`: default result limits.
- Corresponding `_HARD` variables: upper limits accepted from tool calls.

The Go server keeps its own generic configuration and can be used directly by any stdio MCP client.

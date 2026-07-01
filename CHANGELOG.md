# Changelog

## 0.2.0

- Remove MCP tool `status` (full-workspace file/LOC walk). Use `explore` or `search` instead.
- Drop unused `sync.Mutex`, `encoding/json`, and dead helpers after status removal.
- MCP server version string `0.2.0`; documented 8-tool surface in README.

## 0.1.0

- Initial 9-tool ripgrep-based MCP surface matching colbymchenry/codegraph shape.
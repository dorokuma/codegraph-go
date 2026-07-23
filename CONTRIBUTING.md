# Contributing

## Versioning

This project follows [Semantic Versioning 2.0.0](https://semver.org/).

Given a version number `MAJOR.MINOR.PATCH`:

- **MAJOR** — incompatible API or tool contract changes (removing/renaming a tool, changing argument semantics, breaking existing MCP clients).
- **MINOR** — backward-compatible new features (new tool, new config option, new language support).
- **PATCH** — backward-compatible bug fixes.

### Rules

1. Every change must be recorded in `CHANGELOG.md` under an `[Unreleased]` section (or directly in the release section if releasing immediately).
2. Releases must be tagged with `vX.Y.Z` (e.g., `v0.6.0`).
3. Breaking changes MUST bump MAJOR. For `0.x` versions, MINOR may carry breaking changes per semver spec, but prefer PATCH for fixes and MINOR for features.

## Development workflow

1. Create a feature branch from `main`.
2. Make your changes. Keep them focused — one change per branch.
3. Build and test: `go build ./... && go test ./...`
4. Update `CHANGELOG.md`.
5. Open a pull request against `main`.

## Code style

- Standard Go conventions (`gofmt`, `go vet`).
- Keep functions focused on one task.
- Avoid adding dependencies without discussion.

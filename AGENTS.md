# AGENTS.md

## 铁律
1. 构建与测试：构建使用 `go build ./...` 或 `go build -o codegraph-go ./cmd/codegraph-go`，测试使用 `go test ./...`，必须保持零编译与测试错误。
2. 依赖控制：SQLite 必须 `modernc.org/sqlite`（纯 Go）；`smacker/go-tree-sitter` 是明示的 CGO 例外（CI 使用 `CGO_ENABLED=1`），禁止把 `CGO_ENABLED=0` 当默认。
3. 接口契约：单一 MCP 工具暴露原则（`codegraph` + `action`），严禁破坏已发布的 action 与参数契约。
4. 架构分层：严格遵守 `cmd/` 入口与 `internal/` 业务分层，禁止在 `cmd/` 堆积业务逻辑。
5. 变更记录：功能改动与契约变更必须在 `CHANGELOG.md` 的 `[Unreleased]` 中登记。

## 索引
- 现状与架构文档：[README.md](README.md)、[CONTRIBUTING.md](CONTRIBUTING.md)
- 决策与踩坑笔记：[.agents/notes/](.agents/notes/)
- 写完笔记刷新索引：scripts/notes-index.sh（本地生成 INDEX.md，不入 git）

## 关联仓库
- `ctxmode`：同为 MCP 工具链组件
- `prism`：同属 AI 基础设施

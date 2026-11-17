# AGENTS.md
档位：L2

## 铁律
1. 构建与检查：`make check` 为唯一检查入口（覆盖 `gofmt -l`、`CGO_ENABLED=1 go vet ./...`、`CGO_ENABLED=1 go test -race ./...`、`CGO_ENABLED=1 go build -o /dev/null ./cmd/codegraph-go`、`staticcheck`），必须保持零编译与测试错误；CI 直接调用 `make check`。宿主机前置依赖：`staticcheck` 需预装到 `$(go env GOPATH)/bin`（`go install honnef.co/go/tools/cmd/staticcheck@2026.1`，与 CI 对齐），`make check` 会先行快检并在缺失时直接报错。
2. 依赖控制：SQLite 必须 `modernc.org/sqlite`（纯 Go）；`smacker/go-tree-sitter` 是明示的 CGO 例外（CI 使用 `CGO_ENABLED=1`），禁止把 `CGO_ENABLED=0` 当默认。
3. 接口契约：单一 MCP 工具暴露原则（`codegraph` + `action`），严禁破坏已发布的 action 与参数契约。
4. 架构分层：严格遵守 `cmd/` 入口与 `internal/` 业务分层，禁止在 `cmd/` 堆积业务逻辑。
5. 变更记录：功能改动与契约变更必须在 `CHANGELOG.md` 的 `[Unreleased]` 中登记。
6. 提交、分支、门禁与收口按 `/root/.pig/agent/SYSTEM.md` §4。

## 索引
- 现状与架构文档：[README.md](README.md)、[CONTRIBUTING.md](CONTRIBUTING.md)
- 决策与踩坑笔记：[.agents/notes/](.agents/notes/)
- 写完笔记刷新索引：scripts/notes-index.sh（本地生成 INDEX.md，不入 git）

## 关联仓库
- `ctxmode`：同为 MCP 工具链组件
## Release Policy / 发布规范

本节是本仓库发版的强制规则。仓库已有的更具体版本、构建或发布渠道规则优先，但不得降低以下底线。

1. **版本与变更记录**：遵循 Semantic Versioning，正式版本使用 `vX.Y.Z`。破坏性变更升 MAJOR，新功能升 MINOR，兼容修复升 PATCH。任何用户可见行为、运行时/API/包接口、依赖、安全性或分发产物变化，都必须在同一发版提交中更新版本元数据并写入 CHANGELOG/Release Notes；纯文档且不产生可分发产物时可免升版，但须在规则或 Release Notes 中明确说明。
2. **发布门禁**：发版前工作树必须干净，版本号、CHANGELOG 与发布说明必须一致；必须运行仓库规定的测试、构建、lint、类型检查及必要的安全检查，且全部通过。若仓库提供 `release-check`、`make release-check`、版本检查脚本或同等门禁，必须原样通过，不得绕过。
3. **发布顺序**：先提交并推送发布提交到目标分支 → 在该发布提交上创建 Tag（默认使用带注解的 `git tag -a vX.Y.Z -m "vX.Y.Z"`；仅当现有发布工具明确要求轻量 Tag 时可例外）→ 推送 Tag → 创建**非草稿** GitHub Release → 再发布 npm、PyPI、容器镜像等关联分发渠道。不得只推分支或只打 Tag 就宣称发版完成。
4. **GitHub Release 产物**：标题使用 `vX.Y.Z`，正文必须包含实质性变更摘要、升级/破坏性变更说明及验证结果；二进制、安装包、插件包等适用产物必须作为 Release assets 上传，并提供校验和（如适用）。**Tag 本身不等于完整发版，必须同时存在可访问的 GitHub Release。**
5. **发布后验收**：至少执行并确认 `git ls-remote --tags origin refs/tags/vX.Y.Z`、Tag 指向发布提交、`gh release view vX.Y.Z` 可访问且不是 draft；若发布到包仓库/镜像仓库，还必须核验对应版本、摘要或安装链接可用。
6. **不可变性**：已发布 Tag 不得复用、移动或删除，不得 force push 发布分支或 Tag，不得从未提交、未经测试或未通过门禁的树发布。发布后发现问题须通过新的 patch 版本修复，禁止覆盖历史 Release。
7. **例外必须显式化**：归档、已停用、被替代、仅文档或无可分发产物的仓库，必须在项目规则中明确写出“不发新版本”及历史 Tag/Release 的处置方式；例外不能用于绕过上述发布闭环。

### Repository-specific release contract

- 本仓具体门禁见 `CONTRIBUTING.md`；发版前必须同步 `CHANGELOG.md`，并使用 `vX.Y.Z` Tag。

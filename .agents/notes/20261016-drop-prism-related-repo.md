---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: tools # db | extraction | resolution | server | sync | tools
---

# 移除 AGENTS.md 中已归档 prism 关联仓库条目

## 一句话结论
- 从本地 `AGENTS.md` 的「## 关联仓库」章节移除已归档退役的 `prism` 条目，仅保留仍在维护的 `ctxmode`；历史笔记 `.agents/notes/20261003-pi-ext-deploy-sync.md` 与源码注释 `internal/server/project.go:57` 按规范保持不改。

## 背景
- `prism` 项目已归档退役并完成全机运行时与 Pi 扩展清理，本仓 `AGENTS.md` 第 18 行仍将其列为活关联仓库（`- `prism`：同属 AI 基础设施`），属于过期文档指引。

## 决策
- 从 `AGENTS.md`（受全局 `/root/.config/git/ignore` 忽略的本地协作文档，见提交 `0c1d905`）的「## 关联仓库」列表移除 `- `prism`：同属 AI 基础设施`，保留 `- `ctxmode`：同为 MCP 工具链组件`。
- 在 `CHANGELOG.md` 的 `[Unreleased] -> Changed` 登记本次文档清理，新增本笔记并运行 `scripts/notes-index.sh` 刷新本地 `.agents/notes/INDEX.md`（`.gitignore` 忽略）。
- 保持历史笔记 `.agents/notes/20261003-pi-ext-deploy-sync.md:101`（记录 2026-10-03 当时的扩展目录快照）与 `internal/server/project.go:57`（源码注释中说明路径前缀匹配规则的英文例句 `"compare this with prism"`）原样不改。

## 被放弃的方案（必填）
- 方案 A：使用 `git add -f AGENTS.md` 强制将 `AGENTS.md` 重新纳入 Git 跟踪。放弃理由：违背提交 `0c1d905`（`chore(git): stop tracking local agent docs`）与全局 `/root/.config/git/ignore` 规则。
- 方案 B：一并删改 `.agents/notes/20261003-pi-ext-deploy-sync.md` 中的 `prism.ts` 提及。放弃理由：违反 `.agents/notes/README.md`「严禁直接改写旧结论」的历史不可篡改原则。

## 来源
- 关联任务：`MARK-PRISM-REFS-20261016`（分支 `chore/remove-prism-reference`）。

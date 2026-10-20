---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: tools # db | extraction | resolution | server | sync | tools
---

# 2026-10-21 A 档门禁审查 consider 留痕（pi-cache-stats 改名路径与 CHANGELOG 豁免裁定）

## 一句话结论

- 登记 `chore/remove-prism-reference` 分支 2026-10-21 A 档门禁审查通过后尚未闭环的 2 条 `consider` 观察项（`.agents/notes/20261003-pi-ext-deploy-sync.md` 中已改名仓 `pi-cache-guardian` 历史路径引用，以及 `CHANGELOG.md [Unreleased]` 对 agent 笔记内部治理提交的豁免登记裁定），明确不阻断理由与后续处置条件。

## 背景

- 在 `chore/remove-prism-reference` 工作分支清理已退役 `prism` 引用及收口 `.agents/notes/20261003-pi-ext-deploy-sync.md` 权威源条目后，经主代理组织的 2026-10-21 A 档门禁审查，应修项（权威源声明第 14-16 行三处指向已删「来源 → 后续项 #3/#4/#5」的悬空括号指代）已修复清零；另有 2 条非阻断 `consider` 项需建档留痕。

## 决策（consider 逐条留痕）

### consider 1：`.agents/notes/20261003-pi-ext-deploy-sync.md:4` 及权威源声明中的改名仓路径与扩展数历史陈述

- **出处**：`.agents/notes/20261003-pi-ext-deploy-sync.md:4`（`supersedes: "/root/workspace/pi-cache-guardian/.agents/notes/20261004-pi-ext-sync-install-script.md"`）及第 11、16 行权威源声明与正文历史记录。
- **问题**：
  1. `supersedes` 与权威源声明中的 `/root/workspace/pi-cache-guardian` 路径因该仓于 2026-10-18 改名为 `pi-cache-stats` 已失效；但跨仓反向指针（`pi-cache-stats` 侧笔记 `superseded_by` 指回本文件）仍有效可追溯。
  2. 此外，`pi-extensions` 联邦中枢纳管扩展数已随 `prism` 退役由 12 收敛为 11，本笔记中「12 扩展」闭合记录属带日期的历史快照陈述。
- **建议**：将 `pi-cache-guardian` → `pi-cache-stats` 路径引用列入遗留，择期在跨仓笔记路径维护轮次中统一刷新改名仓引用（或加注改名说明）；「12 扩展」带日期历史闭合记录按 `.agents/notes/README.md` 历史不可篡改原则保持不改。
- **不阻断理由**：跨仓反向指针（`pi-cache-stats` 侧 `superseded_by` 指回本文件）仍完整有效、链路可追溯，且「12 扩展」属当时（2026-10-16）带日期的真实历史记录，不影响本分支移除 `prism` 活引用及权威源去重目标。
- **后续处置条件**：后续若发起跨仓 `pi-cache-guardian` → `pi-cache-stats` 历史指针统一刷新专项维护，或修订三仓同源 `install.sh` 权威源台账时，一并更新或加注 `supersedes` 与声明中的新仓路径 `/root/workspace/pi-cache-stats`；「12 扩展」历史快照继续保持原样不改。

### consider 2：`CHANGELOG.md [Unreleased]` 未登记本批 agent 笔记内部治理提交的口径裁定

- **出处**：`CHANGELOG.md` `[Unreleased]` 章节（对比本分支提交 `25dcc23` 与 `fa7c87e` / `7546086` / `182ef77` / `bb733e7` 及待提交 diff）。
- **问题**：提交 `fa7c87e`、`7546086`、`182ef77`、`bb733e7` 及本次 `.agents/notes/20261003-pi-ext-deploy-sync.md` 待提交 diff 均未在 `CHANGELOG.md [Unreleased]` 登记，而同分支早先提交 `25dcc23`（同为本地协作文档 `AGENTS.md` 清理）已在 `CHANGELOG.md` 登记，表面口径存在不一致。
- **建议**：明确区分「仓库级关联项目指引清理（`25dcc23`）」与「`.agents/notes/` 内部台账去重与措辞收口」，由主代理做出统一豁免裁定并记入笔记留存。
- **不阻断理由**：主代理已正式裁定——本批（`fa7c87e` / `7546086` / `182ef77` / `bb733e7` 及待提交 diff）純属 `.agents/notes/` agent 笔记内部治理与去重收口，无任何产品功能、CLI、MCP 接口或部署脚本行为变更，豁免在 `CHANGELOG.md` 登记，理由正式记入本笔记。
- **后续处置条件**：已由主代理裁定豁免并借本笔记完成书面闭环，后续无需补入 `CHANGELOG.md`；未来仅当涉及对外可见行为、部署脚本契约或仓库级文档策略变更时才在 `CHANGELOG.md` 登记。

## 被放弃的方案（必填）

- **方案 A**：在本次修复悬空指代时顺手改写 `20261003-pi-ext-deploy-sync.md` 的 `supersedes` frontmatter 及「12 扩展」历史数字。放弃理由：本轮第一部分严格限定仅删除第 14-16 行三处「来源 → 后续项 #3/#4/#5」悬空指代、严禁改动 frontmatter 与历史闭合记录；且历史快照数字按规范不应篡改。
- **方案 B**：将 `fa7c87e` 等纯内部笔记去重提交逐条塞入 `CHANGELOG.md [Unreleased]`。放弃理由：造成面向用户的变更日志被内部协作笔记维护噪音污染，不如按主代理裁定豁免并在笔记中备案理由。

## 来源

- **审查来源**：主代理组织的 2026-10-21 A 档门禁审查结论（分支 `chore/remove-prism-reference`）。
- **知情状态**：用户已过目。

---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: tools # db | extraction | resolution | server | sync | tools
---

# Pi 扩展同步收进 deploy.sh：消灭「二进制有部署、扩展靠手工」的漂移

## 一句话结论
- Pi 扩展（integrations/pi/codegraph-go.ts）的同步从 README 手工 install 收进独立脚本 `integrations/pi/install.sh` 并由 deploy.sh 调用；幂等（md5 同则跳过不重写）、失败必须有可见输出、deploy.sh 结尾有 WARN 汇总，杜绝「部署成功但扩展是旧版且无人知晓」。

## 背景
- deploy.sh 只做：编译 → 替换二进制 → 停旧 daemon → 预热 → （可选）提交。Pi 扩展的同步只写在 integrations/pi/README.md 的手工 install 一行，没有任何机制保证它被执行。
- 事故实证：`/root/.pi/agent/extensions/codegraph-go.ts` 实际落后仓库一个提交——旧版 `formatCleanText` 无条件剥代码围栏与 rg fallback 的 `file:line:content`，把 `action=node` / `includeCode=true` / `skipCode=false` 本应保留的源码正文二次剥掉，用户在 Pi 里拿不到代码。而仓库 HEAD 版已带 `wantsKeepCode` 逃生口（`cleanOpts` 在调用点按 action/参数决定 keepCode）。
- 部署副本里还有一处注释路径修正（`/root/codegraph-go` → `/root/workspace/codegraph-go`）从未回灌仓库——手工同步是双向腐烂的。
- R2 双审（reviewer + oracle）对 install.sh 失效路径实测出三个静默形态：PATH 无 md5sum 时静默 exit 127、stdout/stderr 全空；HOME 未导出时被 `set -u` 一句晦涩报错打死；`PI_EXT_DEST` 指向目录时 `install` 把文件静默拷进目录、打印 `changed: installed /tmp` 且 exit 0。

## 决策
- 同步逻辑放独立小脚本 `integrations/pi/install.sh`（与 scripts/notes-index.sh 的惯例一致，且可单独触发验证），deploy.sh 在「替换二进制」之后插入 `=== 同步 Pi 扩展 ===` 段调用它。
- install.sh 契约：
  - 目标 `DEST` 语义定稿为**文件路径**（`${PI_EXT_DEST:-$HOME/.pi/agent/extensions/codegraph-go.ts}`）。指向已存在目录 → 显式 `FAILED`，不静默补全文件名、不静默拷进目录（修前实测就是静默拷进 `/tmp/codegraph-go.ts` 还报 exit 0）。
  - 幂等：源与目标 md5 相同 → 报告 unchanged 且不重写（mtime 不变），实测有效。
  - 失败必须可见：预检 `command -v md5sum`；`md5_of` 不吞 stderr/退出码；源或目标 md5 取空一律 `FAILED: ...` 到 stderr 并 exit 1；HOME 为空给可操作提示（而非 set -u 的「未绑定的变量」）；DEST 必须是绝对路径。
  - 打印源/目标 md5 与是否变化；提示 Pi 需 `/reload` 或新会话才生效。
- deploy.sh：
  - 同步步骤失败**不阻断**二进制/daemon 主体（仅 WARN），需要硬失败可单独运行 install.sh；
  - 结尾（`=== 完成 ===` 之后）加 WARN 汇总重打——原事故本质是「漂移发生时没有任何信号」，中段一行 WARN 会被后续输出冲掉；成功路径退出码仍为 0，语义不变；
  - `DEPLOY_COMMIT=1` 的 git add 列表补入 `integrations/pi/codegraph-go.ts`（修前只加了 install.sh 没加它要安装的本体，DEPLOY_COMMIT 收尾会把扩展改动静默留在工作区，复刻同类事故）。

## 被放弃的方案（必填）
- 方案 A：把同步逻辑内联进 deploy.sh（无独立脚本）。放弃理由：失去可单独触发验证的能力，deploy.sh 已 19KB 且双审要求「该步骤能被单独触发以便验证」；独立脚本与仓库 scripts/notes-index.sh 惯例一致。
- 方案 B：install.sh 对 `PI_EXT_DEST` 指向目录时自动补全为 `<dir>/codegraph-go.ts`。放弃理由：静默改写用户给的路径是另一种歧义（显式失败优于魔法），且 `install` 拷进目录的行为正是要消灭的静默形态。
- 方案 C：同步失败让 deploy.sh 整体 exit 非零。放弃理由：硬边界要求「不影响现有二进制与 daemon 部署流程」；改为结尾 WARN 汇总 + 退出码保持 0，把「可区分性」放在输出而非码上。

## 来源
- 关联：分支 chore/pi-ext-sync（未提交）；CHANGELOG.md [Unreleased]；R2 修复任务 FIX-CODEGRAPH-PI-EXT-SYNC-R2-20261003。
- 后续项（不阻断本次但不该被忘）：
  1. 同一扩展目录 `~/.pi/agent/extensions/` 下还有 4+ 个自研扩展（ctxmode.ts / cache-guardian.ts / prism.ts / herdsman-pi.ts / herdr-agent-state.ts）各踩同一颗雷，本仓库管不到，需各自归属仓库补齐同步机制。
  2. deploy.sh 仍无 dirty-tree / 分支检查——它会把未审查的 ts 直接盖到生产扩展目录；后续应加工作区洁净度或显式确认。
  3. 扩展加载无版本戳：运行中的会话无法自证新旧，`/reload` 只能靠人工确认；可在扩展里暴露版本常量供 status 类 action 输出。
  4. `PI_EXT_DEST` 语义已定稿为文件路径（见上），README 若补充文档需保持同一口径。

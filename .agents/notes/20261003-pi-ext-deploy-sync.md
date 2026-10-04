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
- R2 双审（reviewer + oracle）对 install.sh 失效路径实测出多个静默形态，其中「md5 取不到」一类有两种：PATH 无 md5sum 时静默 exit 127、stdout/stderr 全空；md5sum 存在但 md5 仍取空时（如源文件读不出）——bash 的 `set -e` 并不拦命令替换内部的管道失败，旧脚本会带着空的 `SRC_MD5` 走进 unchanged 分支、静默跳过同步（来源：oracle 复审实测：`no-set-e: SRC_MD5=[] reached exit=0` 与 `set-e on: REACHED after assignment exit=0`）。新版 guard 对两种形态分别防住：形态①由 `command -v md5sum` 预检在算 md5 之前直接显式 FAILED（不再是 exit 127、输出全空）；形态②由 `SRC_MD5`/`DEST_MD5` 赋值后立即 `[ -n ... ] || fail` 断言非空、且 `md5_of` 不再吞 stderr/退出码，脚本根本走不到 unchanged 比较。其余形态：HOME 未导出时被 `set -u` 一句晦涩报错打死；`PI_EXT_DEST` 指向目录时 `install` 把文件静默拷进目录、打印 `changed: installed /tmp` 且 exit 0。

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

## 追加修复（2026-10-04）：PI_EXT_DEST 与 HOME 校验顺序倒挂 + 目录报错文案

> 双审在 `pi-cache-guardian` 副本上发现，本样板逐字对齐、缺陷同款存在。仅修 install.sh 两处，未重构脚本。分支 `fix/pi-ext-install-home-order`（双审第二轮：reviewer 判「可放行」、oracle 判「放行」，随本分支提交一并登记）。

### 缺陷形态
- 目标解析「先校验 HOME 再取 PI_EXT_DEST」，顺序倒挂：

  ```bash
  HOME_DIR="${HOME:-}"
  [ -n "$HOME_DIR" ] || fail "HOME is not set (or empty) — export HOME or set PI_EXT_DEST explicitly"
  DEST="${PI_EXT_DEST:-$HOME_DIR/.pi/agent/extensions/codegraph-go.ts}"
  ```

  `HOME` 未设置时先 `fail`，**显式设置 `PI_EXT_DEST` 也照样被拦**，而报错文案偏偏教用户「set PI_EXT_DEST explicitly」——文案与行为自相矛盾。实测 `env -u HOME PI_EXT_DEST=/tmp/x.ts bash install.sh` → FAILED、exit 1。
- 目录报错文案 `(dirname 而非目录本体)` 在 shell 语境下易被读成「父目录」，语义含糊。

### 影响面
- 四处 `install.sh` 契约副本同款：`pi-cache-guardian`、`ctxmode`、本样板（本仓）、在建新仓 `/root/workspace/pi-extensions`（后者以 `DEST_DIR` 形态，其「仅未指定覆盖时才校验 HOME」已是正确顺序）。**「四处同款」是对缺陷历史的陈述，不等于四处都已修复**：各副本由其归属仓的修复轮分别对齐，本仓（codegraph-go）已完成（见下「修复口径」），其余副本以其各自归属仓的提交为准；本条作为契约权威记录登记。

### 实测证据
- 修复前：`env -u HOME PI_EXT_DEST=/tmp/pi-ext-fix/codegraph-go.ts bash integrations/pi/install.sh` → FAILED、exit 1。
- 修复后：同命令 → `changed`（exit 0）；同目标再跑一次 → `unchanged`（exit 0）；临时目标 md5 与源一致（`fc35f79118e4b655f07a4fadf793f5dd`）。
- 仍须失败（四条均 stderr 显式 FAILED、exit 1、stdout 空）：`env -u HOME`（未给 PI_EXT_DEST）；`PI_EXT_DEST=relative/x.ts`（非绝对路径）；`PI_EXT_DEST=/tmp`（目录，新文案 `(须指定文件名而非目录本体)`）；受限 PATH 无 md5sum（`md5sum not found in PATH`）。
- 真实目标幂等：不带覆盖变量跑一次 → `unchanged`，`/root/.pi/agent/extensions/codegraph-go.ts` mtime 前后不变（`2026-10-03 22:34:06.492614208 +0800`，inode `19959456`）。

### 修复口径
- 目标解析改为「优先采纳显式覆盖，仅在未指定时才回落 HOME」：

  ```bash
  if [ -n "${PI_EXT_DEST:-}" ]; then
    DEST="$PI_EXT_DEST"
  else
    HOME_DIR="${HOME:-}"
    [ -n "$HOME_DIR" ] || fail "HOME is not set (or empty) — export HOME or set PI_EXT_DEST explicitly"
    DEST="$HOME_DIR/.pi/agent/extensions/codegraph-go.ts"
  fi
  ```

- 目录报错文案统一为 `expected the extension file path (须指定文件名而非目录本体)`。
- 其余语义一律不动：`command -v md5sum` 预检、`md5_of` 不吞 stderr/退出码、源或目标 md5 取空即败、DEST 必须绝对路径、目标为目录显式 FAILED、md5 幂等不重写不动 mtime、结尾 `/reload` 提示。

### 遗留项（双审第二轮要求登记，2026-10-04）
1. **`ctxmode` 副本尚未对齐**：仍是旧倒挂顺序 + 旧文案 `(dirname 而非目录本体)`；且它是新仓 `/root/workspace/pi-extensions` 的**联邦级联宿主**——中枢级联会显式传覆盖变量（形如 `env PI_CTXMODE_EXT=... <宿主脚本>`），在 `HOME` 缺失环境下旧顺序会被直接拦死，即「级联显式给了覆盖变量、却被 HOME 校验拒之门外」。该副本由其归属仓的修复轮处理，不阻塞本仓本次提交。
2. **目标带尾部斜杠且父目录不存在时，报错口径不统一**（既有缺陷，非本次引入）：`PI_EXT_DEST=/tmp/x/` 时 `mkdir -p "$(dirname '/tmp/x/')"` 建的是父级 `/tmp`，随后 `install -m 644 ... '/tmp/x/'` 原样报错，**不是统一的 `FAILED:` 前缀**——实测 `env -u HOME PI_EXT_DEST=/tmp/pi-ext-fix-banana/ bash integrations/pi/install.sh` 在打完 `destination: /tmp/pi-ext-fix-banana/` 的 stdout 块后，stderr 为 `install: 无法创建普通文件 '/tmp/pi-ext-fix-banana/': 不是目录`，exit 1。修前同样形态（`install` 调用一直未被包裹），不属本次修复范围；若后续要把失败语义收敛成统一口径，需同时顾及 `dirname` 对尾部斜杠的剥除行为。
3. **新仓 `pi-extensions` 的 registry 级联契约**：应把「覆盖变量优先于 `HOME`」写成显式级联契约（含每个宿主脚本的覆盖变量名），并考虑在 `--audit` 增加契约探针——以 `env -u HOME PI_EXT_DEST=<临时目标> <宿主脚本>` 干跑探测顺序倒挂，把「逐字对齐样板」从人肉纪律变成可巡检项。

## 来源
- 关联：分支 chore/pi-ext-sync（未提交）；CHANGELOG.md [Unreleased]；R2 修复任务 FIX-CODEGRAPH-PI-EXT-SYNC-R2-20261003。
- 后续项（不阻断本次但不该被忘）：
  1. 同一扩展目录 `~/.pi/agent/extensions/` 下还有 4+ 个自研扩展（ctxmode.ts / cache-guardian.ts / prism.ts / herdsman-pi.ts / herdr-agent-state.ts）各踩同一颗雷，本仓库管不到，需各自归属仓库补齐同步机制。
  2. deploy.sh 仍无 dirty-tree / 分支检查——它会把未审查的 ts 直接盖到生产扩展目录；后续应加工作区洁净度或显式确认。
  3. 扩展加载无版本戳：运行中的会话无法自证新旧，`/reload` 只能靠人工确认；可在扩展里暴露版本常量供 status 类 action 输出。
  4. `PI_EXT_DEST` 语义已定稿为文件路径（见上），README 若补充文档需保持同一口径。

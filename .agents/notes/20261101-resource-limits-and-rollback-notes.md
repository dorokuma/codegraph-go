---
status: active
superseded_by: ""
supersedes: ""
模块: "daemon / config / extraction"
---

# 资源限制的边界与旧二进制回退行为（O1 观察项）

## 一句话结论
- 四个资源旋钮是**四层分工**而非互相替代：`index_workers` 限任务并发、`max_procs` 限 Go 调度宽度（不覆盖 CGo OS 线程）、`nice` 降 CFS 调度权重（软让路，空闲不限速；Linux 下为有界多轮收敛的 best-effort）、`cpu_quota_percent` 在 cgroup 层给跨 Go/CGo 全线程的硬带宽上限。第四层只能由 `SpawnDetached` 生效，**任何拉起 daemon 的入口（包括 `deploy.sh` 预热）都必须走 `SpawnDetached`/`-warm-daemon`**，否则配额形同虚设。旧二进制回退时 `yaml.v3` 静默忽略未知键，不报错。

## 背景
- C 档提交门禁第 1 轮观察项 O1：新增的 `nice` / `max_procs` / `index_workers` / `cpu_quota_percent` 四个资源旋钮各自边界与回滚兼容性需要书面记录，避免后续误配或误判。

## 决策 / 事实记录
- **四层限流分工（互不替代）**：
  1. `index_workers`：限制 AST 抽取任务的并发 worker 池（任务级并行度），由 `internal/extraction` 的 worker 池直接消费；
  2. `max_procs`（`GOMAXPROCS`，钳制 [0,64]）：限制同时执行 Go 代码的 P 数量；但 goroutine 阻塞在 CGo `go-tree-sitter` 调用时占用的 OS 线程（M）**不计入** GOMAXPROCS——它只钉住 Go 侧调度宽度，不能把 AST 提取的 CPU 总量严格限在 N 核；
  3. `nice`（钳制 [0,19]）：`setpriority(2)` 降低 CFS 调度权重，仅在宿主机 CPU 争用时让出算力，空闲时**不限速**。Linux 下 nice 按线程生效，仅 `PRIO_PROCESS,0` 只会 nice 调用线程——实现先做一次进程级调用，再有界地多轮扫描 `/proc/self/task/<tid>` 对每个线程单独 `setpriority`：每轮重读目录且只处理未见过的 tid，连续一轮无新 tid 即收敛退出，最多 `setNiceRounds = 8` 轮（上限只为防无限克隆线程的负载把启动循环变成无限旋转；`ESRCH` 忽略）。**语义是有界多轮收敛的 best-effort，不是「覆盖全进程所有线程」的硬保证**：最终轮之后新开的线程靠「子线程继承父线程 nice」兼容，极端并发 CGo 线程派生（或已达轮次上限）仍有漏网窗口；要把带宽真正钉住只能靠下一层。
  4. `cpu_quota_percent`（钳制 [0,6400]，如 `150` = 1.5 核）：经 `systemd-run --scope -p CPUQuota=<N>%` 在 cgroup v2 层对**整个进程树全部线程**提供硬带宽上限，是唯一能把 CGo 线程也钉死的旋钮。
- **systemd-run 失败与时序边界（如实边界，勿夸大）**：
  - `cpu_quota_percent` 仅在 euid=0 + Linux + `/run/systemd/system` 存在 + `systemd-run` 在 PATH 时生效，其余环境（容器、非 systemd 主机、非 root 会话）自动落回普通 spawn；
  - `cmd.Start()` 同步失败（如权限、cgroup v1-only 主机、transient unit policy）立即同调用回退直接 spawn；
  - scope 异步创建失败且在 150ms 观察窗内非零退出、且尚无存活锁持有者（`daemonAliveAt` 读取 pidfile 并经 `IsProcessAlive` 判活；DB 级 flock 互斥由守护进程或直连回退打开索引库时的 `db.Open` / `checkDirectFallbackSafe` 兜底），同样同调用回退直接 spawn；锁竞争输家 0 退出与赢家持锁两种正常路径都不会误触发回退；
  - 若 D-Bus 拥塞导致 >150ms 才异步失败：spawn 已按"正常启动"返回，后台 reaper goroutine 捕获到非零退出只记 `slog.Warn("systemd-run scope exited with error after startup window")`；客户端侧经 `EnsureAndDial` 6 秒拨号超时后安全落回 direct 模式——DB flock + pidfile 兜底防双写，小窗 TOCTOU 下即使偶发双拉起，抢锁输家 0 退出；
  - 未加 `--collect` 的瞬态失败 scope 可能在 systemd 单元列表中短暂残留（`systemctl list-units` 可见），属供排障的无害残留。
- **旧二进制回退**：回退到不含新字段的旧版本时，`gopkg.in/yaml.v3` 默认忽略未知 YAML 键，`nice` / `max_procs` / `index_workers` / `cpu_quota_percent` / `exclude_dirs` 留在 config.yaml 里不会导致旧版本解析失败，只是静默不生效。若新旧版本间存在 schema（索引库结构）差异，由启动时的版本检查自动触发全量重建或恢复备份索引库，不需人工干预。
- **排除规则收窄单一真源**：启动时 workdir 收窄（会话落在被排除子树时 Workdirs 收敛为 `[target]`）只经 `internal/extraction.NarrowExcludedWorkdirs` → `ShouldSkipDirIn` 一条链路；`internal/config` 不再持有手抄规则副本，`config.Config.ExplicitWorkdir` 负责保留"用户显式传了 -workdir"的信息以区分 cwd 回退。

## C 档返修闭环（20261102 Round 1）

- **授权根不再被无关旋钮抬高**：`WorkdirAllowlist` 改用只含 `workdirs` 的最小结构 `workdirsOnlyConfig` 解析。旧版反序列化完整 `yamlConfig`，一个非权限键的错值（`nice: invalid` 等）就会让整份 YAML 解析失败→权限根丢弃→退回 `$HOME`，把「只授权 `/root/workspace`」隐式放宽为整份 home（安全边界扩大）。YAML 语法本身损坏时仍按既有契约退回 `$HOME`。
- **`exclude_dirs` 只有一种解释**：新增 `extraction.RgExcludeGlob` 作为 walker 与 rg 之间的唯一转换层——继续用 `filepath.Match` 的语义分类规则（裸名 / 路径 / 顶层锚定），转义 rg 会展开而 `filepath.Match` 不认的花括号，并把 `filepath.Match` 拒收的模式（`data[`、`[a-]`、悬空转义 `a\`）整串按字面目录名转义。实测补充事实（ripgrep 14.1.1，`!\<glob>/` 目录形）：`{` 未闭合与 `[` 未闭合都直接 exit 2，**结尾反斜杠为偶数也报错（`a\\/`），奇数则合法**（末尾 `\` 把 `/` 转义掉），故转换层只在偶数结尾时补一个 `\`。唯一无法表达的情形（`a\` 这类悬空转义规则）退化为空操作：宁可不排除，不可误排除，更不可报错。〔注：该「空操作」表述已被下方第 4 轮复审取代——`a\` 现经 `a[\\]` 字面类精确排除，不再退化。〕
- **回退路径不再重复拉起**：`systemd-run` 包装的 `cmd.Start()` 同步失败时回退直接启动，必须同时 `quotaArgs = nil`；否则直接启动的子进程也进入 150ms 异步探测窗，而该窗「pidfile 无存活者」的条件会把一个非零退出的启动失败误判为 scope 创建失败→再拉起一次（实测：修前假 daemon 被 exec 两次）。
- **预热只剩一条路**：`deploy.sh` 不再 `CODEGRAPH_DAEMON_INTERNAL=1 setsid "$BINARY"`，而是 `"$BINARY" -workdir <wd> [-config <cfg>] -warm-daemon`，走客户端正常链路后由 `SpawnDetached` 拉起（`CPUQuota` 与两级回退同时生效）。实证：预热出的 daemon `cgroup` 为 `system.slice/run-*.scope`，`cpu.max = 150000 100000`（150%）——旧预热路径下该文件根本不存在。就绪探测相应改为**以 pidfile 记录的 daemon PID 为准**（叠加 `daemon_matches` 全套谓词），不再与客户端命令自身的 `$!` 比对（systemd-run 包装后两者恒不相等）。
- **二进制不可变锁**：staged 新二进制 `install -m 0555`，`mv` 前 `chattr -i`、`mv` 后 `chattr +i`，并注册 `EXIT` trap 中途异常退出时恢复「存在 + 0555 + 不可变」；`CODEGRAPH_SKIP_IMMUTABLE_LOCK=1` / 非 root / 无 chattr / 文件系统不支持时逐级降为纯 0555（实测 tmpfs 以外的 ext4 可用，/tmp 测试目录需跳过）。测试：`scripts/test_deploy_immutable_lock.sh`（抽取 deploy.sh 原文段跑，避免副本漂移）。
- **窄 daemon 不再空占 5 分钟**：`Daemon.watchRootLiveness` 每 15s `os.Stat(d.root)`，目录不在且无活跃会话即 `Stop("workdir removed")`（.wt worktree 被删的常见场景）。该 goroutine 不能进 `d.wg`：它自己调 `Stop`，而 `Stop` 等 `d.wg`，进去就死锁。
- **`init <root>` 与 `main()` 规范化对齐**：同一份 `canonicalize(abs)`（Abs + EvalSymlinks）同时给 `ValidateWorkdirs` 与 `db.Open`，避免符号链接 root 在链接位置多建一份 `.codegraph`。

## 被放弃的方案（必填）
- 用 cgroup v2 手动写 cpu.max 替代 systemd-run：放弃——需要守护进程自己管理 cgroup 节点，复杂度和失败面远大于一次性 transient scope，且非 systemd 主机照样要回退。
- 把 `GOMAXPROCS` 当作 CPU 硬上限文档化：放弃——对 CGo 路径是误导性承诺，只能写清楚「调度宽度」语义。
- 把 `setNice` 写成「遍历全进程所有线程」的硬保证：放弃——它只能有界收敛，极端并发下仍有 best-effort 窗口；硬上限属于 cgroup 层的 `cpu_quota_percent`。
- 为 `WorkdirAllowlist` 把整份配置拆成逐字段宽容解析：放弃——越界面只靠「最小读视图」就能消除，逐字段解析会把未知键兼容、钳制与报错文案三套契约重做一遍。
- `spawn` 回退窗口无限等待 systemd-run 结果：放弃——D-Bus 拥塞下会拖死守护进程启动；150ms 窗口 + reaper warn + 客户端拨号超时回退已覆盖慢失败，代价仅是日志而非可用性。

## C 档第 6 轮复审：归一化单一真源收口 + 升级回归指引 + deploy.sh 锁窗前置（20261103）

- **`ShouldSkipDirIn` 与 `RgExcludeGlobs` 共享归一化（must fix）**：r5b 在 `RgExcludeGlobs` 用 `strings.TrimLeft`/`TrimRight` 剥除重复前导/尾随 `'/'`（`"a//"`→`"a"`、`"./a//"`→锚定 `"a"`、`.//a`→锚定 `"a"`、`"//"`→空早退），但 `ShouldSkipDirIn` 仍用单次 `TrimSuffix`/`TrimPrefix`，`"a//"`、`"./a//"`、`".//a"`、`"//a"` 在 walker 侧因残留 `'/'` 永不命中而 rg 侧已排除——两引擎真实分裂。修复：抽出共享辅助 `normalizeExcludeRule(raw) → (pat, anchored, ok)`（`ToSlash(TrimSpace)` → 判定 `./`/`/` 锚定 → 剥锚定前缀与全部前导 `'/'` → 坍缩全部尾随 `'/'` → `""`/`.`/`..` 早退）；`ShouldSkipDirIn` 改调它并以 `!anchored && !Contains(pat,"/")` 走裸名分支、其余（`anchored || Contains(pat,"/")`，与 `RgExcludeGlobs` 的 `allowSlash` 完全同构）走 rel 路径分支；`RgExcludeGlobs` 删除自己的双份裁剪逻辑直调同一函数。新增回归：`skip_test.go` `TestExcludeDirsTopLevelAnchor` 尾段 + `rg_exclude_test.go` `TestRgExcludeArgsWeirdRulesAgreeWithWalker` 补充 `trailb//`/`./anchc//`/`.//anchd`/`//anche//`/`./.`/`./..` 规则与 `trailb`/`sub/trailb`/`anchc`/`sub/anchc`/`anchd`/`sub/anchd`/`anche`/`sub/anche` 候选目录，真实 `rg --files` 逐目录 100% 一致。
- **ripgrep 14.1.1 升级回归指引（should fix）**：README 的 `exclude_dirs` 说明段与本笔记显式记录词法验证基线（见上方第 5 轮条目的 ripgrep 词法验证基线段）；升级本机 `ripgrep` 后运行 `TestRgExcludeArgsWeirdRulesAgreeWithWalker` 校验。
- **`deploy.sh` 替换分支 `protect_binary` 再前置（consider）**：`protect_binary=1` 从 `cp -pf`/`install` 前再提前至 `if [ -f "$BINARY" ]` 内部第一行——预清理残留 `.old`（`unlock_binary` + `rm -f`）期间收到外部信号同样走 `relock_binary` 兜底，覆盖上一条授权命令落不了的更窄窗口。

## 来源
- 关联分支：`feat/exclude-dirs-and-cpu-limit`；相关实现 `internal/daemon/spawn.go`、`internal/daemon/limits.go`、`internal/daemon/limits_linux.go`、`internal/config/config.go`、`internal/extraction/skip.go`。

## C 档第 1 轮复审观察项与证伪记录（20261102）

- **C1（`workdirsOnlyConfig` 空列表契约）**：`workdirs: []` 或元素展开后全空时按既有文档契约退回 `$HOME`，属设计预期而非解析失败退化——与 YAML 语法损坏时的既有行为同一路径，不为本轮改动引入的新退化。
- **C2（`RgExcludeGlob` 反斜杠边界）**：`exclude_dirs` 以 `/` 区分单段名与多段相对路径；若单段目录名含尾随悬空 `\`（如 `a\`）或嵌入反斜杠转义，无法在 gitignore 目录 glob 中无歧义表达时退化为空操作（宁可多搜不误杀、不令 `rg` 报错）。〔注：「`a\` 退化为空操作」已被下方第 4 轮复审取代——`a\` 现经 `a[\\]` 字面类精确排除字面目录。〕
- **C5（`watchRootLiveness` 单次 `ENOENT`）**：本地文件系统下 `errors.Is(err, fs.ErrNotExist)` 已排除 `EACCES`/`EIO`，针对本地 `.wt` worktree 删除场景单次确认即释放锁；网络文件系统瞬断不在目标运行模型内。
- **S1 实测证伪备忘**：`internal/server/tools.go` 以 `cmd.Dir = projRoot` 调用 `rg` 传入绝对 `searchRoot` 时，经 ripgrep 14.1.1 实测（含 `searchRoot = <projRoot>` 与 `searchRoot = <projRoot>/ref`），`ignore` 引擎自动对位于 `cwd` 下的绝对路径做 `strip_prefix(cwd)`，`!/ref/` 精确排除顶层 `<projRoot>/ref` 而不误伤 `<projRoot>/pkg/ref`，无需改动实现。

## C 档第 3 轮复审：3 项真缺陷根治 + 观察项归档（20261103）

- **缺陷一（walker↔rg glob 词法等价）**：`RgExcludeGlobs` 新增——一条规则可发射「通配翻译 + 纯字面量翻译」两个 glob，覆盖 `ShouldSkipDirIn`「字面精确相等 OR `filepath.Match`」双语义（`\{foo`、`[ab]`、`[!a]` 这类合法 Go 模式的字面目录名不命中其自身模式，但会被 `name == pat` / `rel == sub` 命中）。`rgSafeGlob` 重写为按 Go `filepath.Match`（Unix）词法逐 token 转换：`\c` 转义对中 `\\`→`[\\]`、`\{`/`\}`→`[{]`/`[}]`（实测 ripgrep 14.1.1：花括号展开在转义解析**之前**，`\{` 仍是 alternation opener → "unclosed alternate group" exit 2；类内 `[{]` 形式不受 brace lexer 影响）；`**` 折叠为 `*`（Go 无 globstar，`a/**/b` 仅匹配单层 `a/x/b`）；`[^a]`→`[!a]`、`[!a]`→`[\!a]`；类外裸 `{}` 一律 `[{]`/`[}]` 化；〔注：`[\!a]` 表述已被下方第 4 轮复审取代——rg 类内 `\` 为字面量，`[!a]` 实发为 `[a!]`。〕`filepath.Match` 拒收的模式整串 `escapeRgLiteral`（`\`→`[\\]`，`{}`→`[{]`/`[}]`，`*?[]`→`\c`），杜绝 `foo\[bar`→`foo\\[bar`（unclosed character class）与 `a\`→`a\\`+`/`（dangling '\'）两种 exit 2。`RgExcludeGlob` 对 `""`/`"."`/`".."` 早退（保留合法目录名 `"..foo"`）。`rgExcludeArgs` 改遍历 `RgExcludeGlobs` 发射负向 `-g` 对。全部经本机 rg 14.1.1 实测：0 个 exit 2，`keep with space`、`[!a]`/`[^a]`、`a/**/b` 三态、`\{foo` 双形态均与 `ShouldSkipDirIn` 一致。
- **缺陷二（无会话退出判据原子化）**：`Daemon` 增加 `mu` 保护的 `inFlight` 计数——`acceptLoop` 在 `Accept()` 成功后同一把锁内查 `stopping`、`inFlight++`、停 idleTimer；`serveConn` 以 `defer finishConn()` 递减（归零且 `clients==0` 时重新 `armIdleTimer`）。`addClient` 改返回 `bool`，停止态拒绝登记、`serveConn` 立即退回。新增原子 `stopIfIdle`：持 `mu` 检查 `clients==0 && inFlight==0` 后 CAS `stopping`、清 idleTimer、取 listener 出锁关闭，再走与 `Stop` 共用的 `finishShutdown`（cancel → 强关 conns → `wg.Wait` → cleanupArtifacts → `close(stopped)` 仅一次）。`watchRootLiveness` 与 `armIdleTimer` 的 `AfterFunc` 均改调 `stopIfIdle`；拒绝时 watchdog `continue`、idle 回调 `armIdleTimer()` 重新排下一拍。回归测试 `TestDaemonStopIfIdleDoesNotCutInFlightOrHandshakingSession` 覆盖：握手期 parked conn（已 Accept 未 ClientHello）下 `stopIfIdle` 返回 `false`、双会话并发下返回 `false`、会话排空后返回 `true` 干净退出且 listener 拒新连接。
- **缺陷三（带空格目录测试假绿 + 对抗集）**：`rg_exclude_test.go` 的 `runRg` 由 `strings.Fields` 改严格 `\n` 切分（去 `\r`、跳过空行），修复 `"keep with space"` 被拆词造成的假绿；新增断言 `kept["keep with space"]` 与 `a/**/b` 隔离态三态验证（`a/x/b` 排除、`a/b`/`a/x/y/b` 保留）。`skip_test.go` 补齐 `\{foo`/`{foo`/`foo\[bar`/`foo[bar` 字面与模式双命中、`./[!a]`/`./[^a]` 顶层锚定类语义、`gr/a/**/b` 子树边界、`".."`.`"."` 早退。
- **oracle 观察项归档**：`exclude_dirs` 的 `"."`/`".."` 已在 `RgExcludeGlobs` 直接早退拦截（不发任何 `-g`，不会误排除搜索根或父级）；`deploy.sh` 不可变锁的 `chattr` 在非 root / 无 chattr / 文件系统不支持（tmpfs）环境逐级降级为纯 `0555`，`scripts/test_deploy_immutable_lock.sh` 相应场景以 `CODEGRAPH_SKIP_IMMUTABLE_LOCK=1` 或能力探测跳过，属设计降级而非缺陷。

## C 档第 4 轮复审：POSIX 字符类规范化 + 单次原子 mv 锁窗收敛 + 观察项归档（20261103）

- **`rg` `globset` 类词法实测与类归一化闭环**：ripgrep 14.1.1 实测——(a) 类内 `\` 是字面量 ASCII 92，从来不是转义（旧版 `[\!a]`/`{\!}` 类内转义写法实际向成员集多塞了一个真反斜杠）；(b) 反向区间 `[z-a]` 直接被拒（"invalid range; 'z' > 'a'"，exit 2），而 Go `filepath.Match` 将其静默视为空集；(c) 类内 `{}`/`,` 不触发花括号展开，原样字面保留即可。闭环实现：`parseGoClass`（Go 词法扫类：`\c` 转义对、`^` 首位否定、成员逐一消费）→ `normalizeRgRangeEndpoints`（剥除区间端点上 rg 位置敏感的 `]`/`-`/`!`/`^`，剥入 singles；lo>hi 丢弃、lo==hi 折叠、区间跨 `/` 不拆——两引擎均允许类区间含 `/`）→ `emitRgClass`（POSIX-ish 发射：`]` 置首为字面量、`!`/`^` 避开首位——正类仅含 `!`/`^` 时降级为类外 `\!`/`\^`/`{\!,\^}`、`-` 置首或置尾、空正类令整条模式不可满足、空否定类由调用方化为 `?`）。净效果：`[!a]`→`[a!]`、`[!]`→`\!`、`[!^]`→`{\!,\^}`、`[a\]b]`→`[]ab]`、`[z-a]`→抑制/`[^z-a]`→`?`，`a\` 类悬空转义规则经 `a[\\]` 字面类精确排除（不再退化为空操作）。
- **`deploy.sh` 锁窗收敛**：替换路径改为单次原子 `mv`——锁内 `cp -pf "$BINARY" "$BINARY.old"` + `install -m 0555 ./bin/codegraph-go "$BINARY.new"`（此时 `$BINARY` 尚未解锁、全程在位），然后**先置位 `protect_binary=1` 再 `unlock_binary "$BINARY"`**（修复旧顺序在两者之间异常中断时 `relock_binary` 因 `protect_binary=0` 早退而漏锁的问题），最后单次 `mv -f "$BINARY.new" "$BINARY"`（rename(2) 单步覆盖，无旧版双 `mv` 的正式路径缺失窗）。`relock_binary` 追加 `.old` 清理兜底：`$BINARY` 仍在位而 `.old` 存在（cp+unlock 后、mv 前中断）时先 `unlock_binary "$BINARY.old"` 再 `rm -f`，任何异常退出绝不遗留 `.old` 文件。`scripts/test_deploy_immutable_lock.sh` 的解锁窗崩溃场景改用独立夹具目录（不复用已被前序用例改写的目录），并在支持不可变位的环境下先 `chattr +i` 模拟生产旧二进制再跑真实模式验证恢复。
- **oracle-cg-r3 `consider` 观察项归档**：`internal/db.FindNearestCodeGraphRootBounded` 在判定"候选 root 是否排除起点"时，候选 root 到起点之间的中间步由 `filepath.Dir` 逐级词法回退产生（纯词法路径运算，发生在任何符号链接解析之前）；主入口（`resolveSessionRoot` 及各调用方）已先做 `canonicalize`（Abs + EvalSymlinks），故词法步数约定不构成安全边界，属留档备查的非缺陷项。

## C 档第 5 轮复审：`*` 后缀非法语法短路 panic + 裸名/路径 `'/'` 分流 + deploy.sh 锁窗前置（20261103）

- **`filepath.Match(pat, "")` 不能当合法性预言机（must fix）**：`match.go` 的 `Match` 在首个含非空前缀的 `*` 分片（`foo*\`、`foo*[`、`foo*[a-`）失配空串后直接 `return (false, nil)`，从未扫描 `*` 之后的分片；于是 `foo*\` 被 `rgSafeGlob` 误判为合法模式进入 `translateRgGlob`，在 `case '\\':` 处 `pat[i+1]` 越界 panic。修复：新增 `isValidGoPattern`，按 `scanChunk` 词法（剥前导 `*`、`\` 跳一字节、`!inrange` 的 `*` 切分 `chunk`）逐段对每个非空 `chunk` 调 `filepath.Match(chunk, "")`，任一报错即 `false`；`RgExcludeGlobs` 与 `rgSafeGlob` 两处裸 `filepath.Match(pat, "")` 均改调它，`translateRgGlob` 另加 `i+1 >= len(pat)` 防御边界。经本机 `rg 14.1.1` 实测：`foo*\`、`foo*[`、`foo*[a-`、`*a\`、`[a]*\` 现按纯字面目录翻译（`foo\*[\\]` 等），无 panic、零 stderr、`fooX` 不误排除而字面目录 `foo*\` 被排除。
- **字符类 `'/'` 按裸名/路径分流（should fix）**：`ShouldSkipDirIn` 裸名分支只拿 `filepath.Base(path)`（绝不含 `'/'`）调 `filepath.Match`，但 rg 的无锚定目录 glob 在 `globset` 中展开为 `**/<glob>`——`[!x]` 与跨 `'/'` 区间 `[.-0-]` 都会命中分隔符，把两层目录 `cmd/codegraph-go` 误排除（实测 rg 14.1.1）。修复：`allowSlash := anchored || strings.Contains(pat, "/")` 从 `RgExcludeGlobs` 经 `rgSafeGlob` → `translateRgGlob` 传入 `parseGoClass`——`allowSlash=false` 时正向区间在 `'/'` 处拆分为 `[lo,'.']` 与 `['0',hi]`（`[\--0]` → `[.0-]`）、非空否定类把 `'/'` 补入 `singles`（`[!x]` → `[!x/]`），`RgExcludeGlobs` 在 `prefix == ""` 且 `strings.Contains(g, "/")` 时自动改用 `"**/"` 前缀保持任意深度生效；`allowSlash=true` 时 `'/'` 是合法成员（`filepath.Match("a[/]b","a/b")` 实测为 true，`getEsc` 不禁止 `'/'`），`parseGoClass` 删去错误的 `r == '/'` 过滤，`a[/]b`/`a[\/]b`/`a[^/x]b`/`a[^z-a]b`/`./[^z-a]` 在 rg 14.1.1 下零 stderr/exit-2 且与 walker 逐目录一致。
- **`deploy.sh` 备份段信号窗收口（consider）**：`protect_binary=1` 从 `then`/`else` 内部提前至 `cp -pf`/`install -m 0555` 之前——中途 `SIGINT`/`SIGTERM` 时 `relock_binary` 同样兜底清理 `.old`/`.new` 并保持 `$BINARY` 为 `0555 + i`。
- **ripgrep 词法验证基线**：全部 `exclude_dirs`→rg glob 转换（类词法、花括号、反斜杠转义、`{?,[/]}` 交替）均针对本机 **ripgrep 14.1.1**（`globset`）实测对齐。升级本机 `ripgrep` 版本后应运行 `go test -p 2 ./internal/extraction ./internal/server -run TestRgExcludeArgsWeirdRulesAgreeWithWalker` 校验兼容性——该测试驱动真实 `rg --files` 逐目录比对 walker 判定。
- **测试与文档同步**：`TestRgExcludeGlobTranslation` 扩至 81 例（新增 `cmd[^x]codegraph-go`→`**/cmd[!x/]codegraph-go`、`a[\--0]b`→`a[.0-]b`、`a[/]b`→`/a[/]b`、`a[^/x]b`→`/a[!/x]b`、`a[^z-a]b`→`a?b`、`./[^z-a]`→`/?`、段间空否定类 `./par[^z-a]b`→`/par{?,[/]}b`、`sub/par[^z-a]b`→`/sub/par{?,[/]}b`、`par/[^z-a]b`→`/par/?b`、`par[^z-a]/b`→`/par?/b`、`par[^z-a]`→`par?`、`par[^z-a]*b`→`par?*b`、`[/]`/`./[/]`→`/[/]`、多斜杠锚定 `///`/`.///`/`//` 早退及尾随斜杠坍缩 `a//`→`a`，与 5 条 `*` 后缀非法语法）；新增 `TestRgExcludeGlobStarSuffixBadSyntax`（不 panic、纯字面翻译、字面目录命中而 `fooX` 不误伤）与 `TestExcludeDirsSlashClassBareVsPath`（裸名 `cmd[^x]`/`cmd[\--0]` 不误杀两层目录但精确排除各深度单段，路径 `par[/]b`/`par[^/x]b`/`par/[^z-a]`/`./[^z-a]` 与 walker 一致）；`rg_exclude_test.go` 同套规则+候选目录进 `TestRgExcludeArgsWeirdRulesAgreeWithWalker`（真实 `rg --files` 逐目录比对，零 stderr/exit-2）。

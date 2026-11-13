# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- **`-warm-daemon` 预热入口**：`codegraph-go -workdir <wd> [-config <cfg>] -warm-daemon` 跑完整客户端启动链（`LoadConfig` → `NarrowExcludedWorkdirs` → `ValidateWorkdirs`）后调 `daemon.EnsureAndDial`，经 `SpawnDetached` 拉起守护进程，连上即挂断连接并退 0（连不上则非零退出，供部署日志记账）。存在理由见下方 Fixed 的部属预热条目。
- **可配置的冗余目录排除**：`config.yaml` 新增 `exclude_dirs`（亦可用 `CODEGRAPH_EXCLUDE_DIRS` 逗号分隔追加），支持四类规则——裸目录名 / glob（如 `*-backup*`，按 basename 匹配）、相对 workdir 的 slash 路径（如 `experiments/scratch`）、以及 `"./sub"` / `"/sub"` 顶层锚定语法（仅排除 `<workdir>/sub` 及其子树，不误伤深层同名目录如 `proj/internal/ref`），统一作用于索引遍历、文件监听、git-assist 的 `ShouldSkipDirIn` 及 rg 侧搜索排除；内置跳过名单新增 `.wt`（git worktree 容器目录），同时新增 workdir 根保护——当 workdir 本身落在被排除路径（含单个 `.wt/<repo>/<branch>` worktree）时根及其子树绝不误跳过。rg 兜底（`search`/`files`/`callers`/`callees`/`impact`）现在经新增的 `extraction.BuiltinSkipGlobs(root)` 单一真源导出同一套内置排除——`skipDirNames` 单段目录名、`skipPathFragments` 多段缓存树（`.local/lib`、`.cargo/registry`、`code_references`、`gopath/pkg` 等）及 Go 工具链段（`go/pkg`、`go/bin`、`go/src`，经 `isGopathRoot` 判定顶层 `go`），不再只排除 `.wt` 与 `exclude_dirs`；`search`/`files` 在已收窄进跳过祖先目录（如 `node_modules/pkg`、`.local/lib/pkg`、`.wt/repo/feat`）的 workdir 下仍正常命中根内文件，不误清空。`index_workers` 亦可经 YAML 配置（优先级：`CODEGRAPH_INDEX_WORKERS` 环境变量 > `index_workers` > 默认 min(8, NumCPU-1)）。
- **守护进程资源限制**：`config.yaml` 新增 `nice`（[0,19]，越界截断；`CODEGRAPH_NICE` 覆盖；Linux 下有界多轮扫描 `/proc/self/task` 收敛，best-effort）、`max_procs`（[0,64] GOMAXPROCS 上限，仅当 `GOMAXPROCS` 环境变量未设置时生效；`CODEGRAPH_MAX_PROCS` 覆盖）与 `cpu_quota_percent`（[0,6400] 钳制，如 `150` 即 1.5 核；`CODEGRAPH_CPU_QUOTA_PERCENT` 覆盖）。`main` 在 `LoadConfig()` 后对当前进程做一行薄调用 `daemon.ApplyProcessLimits`，客户端/守护进程/direct 模式同时生效；`cpu_quota_percent` 仅在 Linux、root（euid=0）、systemd 主机且 `systemd-run` 可用时经 `systemd-run --quiet --no-ask-password --scope -p CPUQuota=<N>%` 包装拉起守护进程，`cmd.Start()` 失败或 150ms 观察窗内非零退出自动无感回退直接执行，更晚的异步失败记 warn 日志并由客户端拨号超时落回 direct 模式。
- **Agent 协作骨架**：接入 `AGENTS.md` 协作规范、`.agents/notes/` 决策与踩坑笔记机制及 `scripts/notes-index.sh` 索引生成工具。

### Fixed
- **Daemon shutdown during post-extraction indexing**: propagate the shutdown signal into full and incremental cross-file resolution (including file/name/empty-tail reference scans and candidate/import SQL lookups), noise cleanup, synthesis graph/node reads and inner loops, and file pruning/directory deletion. Check shutdown before starting the optional Git probe. Once cancellation is observed, do not initiate new file-deletion or resolution transactions or per-reference fallback writes; already-started transactions are not context-bound and complete atomically. An `ApplyResolvedRefs` transaction already in flight when cancellation occurs may still resolve its batch: some references can be resolved while the remainder stays pending, and a declining pending count does not mean references were lost. Interrupted passes leave unflushed references retryable, preserve the last complete synthesized edge set, and return `ErrIndexInterrupted` rather than marking the schema revision. Regression tests cover cancellation during SQLite evaluation, buffered-node iteration, resolution with an unflushed plan, `classMethods` inside the React synthesis pass, deletion between atomic file transactions, and watcher `WaitIdle` draining only the in-flight deletion unit. This prevents idle-timeout cleanup from waiting for the entire reference or deletion backlog after the socket has already closed.
- **`daemonAliveAt` pidfile 诊断读取不再信任叶类型（O1）**：`watchDaemonExit` 的「无活 daemon 记录」探测此前用 `os.ReadFile(PidPath(root))` 按路径读 pidfile——`.codegraph/daemon.pid` 若为无写端 FIFO/设备，open(2) 永久阻塞，watcher 在子进程已回收后卡死、`defer logFile.Close()` 永不执行、诊断 warn 与日志 fd 一并丢失；若为叶软链则跟链读项目外文件，一份指向活进程的伪造 LockInfo 即可错误抑制 warn。改为单次 `unix.Open(pidPath, O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC)`：`O_NOFOLLOW` 让内核在 open() 时对叶软链返回 ELOOP（不跟链出项目）、`O_NONBLOCK` 使 FIFO/设备叶的打开立即返回，随后在同一已绑定 fd 上 `f.Stat` 复核普通文件再 `io.ReadAll`——消除了 Lstat→ReadFile 的叶子换绑窗口；打开失败/非普通叶/读错一律降级为「无活 daemon 记录」，不改 `IsProcessAlive`/`ProcStart` 判定、不动 `TryAcquireLock`/`ClearStaleLock`/`KillStaleDaemon`/proxy/recovery 的 pidfile 读面。回归：`TestWatchDaemonExitFifoPidfileDoesNotBlock`（写端缺失 FIFO 下 watcher 有界返回、warn 仍带 exit_code/log_tail）、`TestWatchDaemonExitRejectsSymlinkPidfile`（指向含活进程合法 LockInfo 外部文件的软链不再误抑 warn）；普通文件正向控制沿用 `TestWatchDaemonExitWarnSuppressedByLiveHolder`。
- **`SpawnDetached` 静默吞掉 daemon 启动失败证据（诊断修复）**：两处证据黑洞——(a) `.codegraph/daemon.log` 打开失败（只读项目 / EACCES）时旧代码置 `logFile=nil` 继续 spawn，子进程 stdout/stderr 被丢弃，父端只看到拨号超时；现在打开失败直接返回携带启动阶段、目标 root、日志路径与 errno 的错误，`EnsureAndDial` 原样传播后安全落 direct 模式，子进程不再被盲拉。(b) 普通 spawn 路径的子进程 `Wait` 结果此前丢进无人消费的 channel，daemon 非零退出（如 `daemon.pid.tmp` / `TryAcquireLock` 写失败）在父端零痕迹；新增 `watchDaemonExit` 统一 reap——子进程带错误退出且 pidfile 未记录存活 daemon 时 `slog.Warn` 只陈述观测事实（wait error、exit_code、root、日志路径）加有界（4KiB）脱敏 `daemon.log` 尾迹；exit 0（锁竞争输家）或 pidfile 记有活 daemon 时静默不误报。告警文案收窄为 `spawned daemon exited; no live daemon recorded in pidfile`——`daemonAliveAt` 只看 pidfile，不推断无 pidfile 的 flock 持锁者（invisible-holder 反例见 `internal/daemon/proxy.go`）。尾迹读取绑定 spawn 时打开 daemon.log 的同一文件身份（`O_RDWR` 打开的 fd 直接 `ReadAt`，不按路径重开）——轮转/rename、软链替换都不会把读重定向到别的文件；日志叶本身也在打开时加固：单次 `unix.Open(logPath, O_RDWR|O_APPEND|O_CREAT|O_NOFOLLOW|O_CLOEXEC, 0600)`——内核在 open() 时对最终叶符号链接返回 ELOOP（不解析、不落地悬空目标），O_NOFOLLOW 仅约束最终叶、中间组件保留 cgdir.Ensure 支持的软链祖先；打开后 `f.Stat` 复核为普通文件（FIFO/目录/设备一律 fail closed），失败关闭自身 fd——软链（活动/悬空/超限目标）与非普通文件（FIFO/目录/设备）一律 fail closed 并保留 EISDIR/ELOOP errno，不再会把子进程输出或尾迹重定向到项目外；轮转判定同样改为 Lstat，软链/非普通叶不再进入轮转。有界窗口读截断处的残缺首行整行丢弃（`token=` 识别前缀恰落在窗口边界外时值片段无法被识别，宁可弃证不泄露），脱敏在完整行文本上执行后再按同一「完整行」规则钳制回 ≤4KiB——修复旧实现字节级二次裁剪把首条幸存记录的识别前缀（如 `Header Basic `）截掉、向生产 safelog 交付不可再脱敏明文片段的组合缺陷：超限即整记录丢弃、单记录超限即安全省略。systemd-run 同步/异步回退后的直接 respawn 复用同一 watcher；150ms 探测窗提取为 `spawnEarlyExitWindow` 常量，scope 相关两级 warn 与 fallback 语义不变，无新增重试。新增/改造回归：`TestSpawnDetachedRejectsSymlinkAndNonRegularDaemonLog`（软链×3+FIFO 拒绝与常规文件控制）、`TestDaemonLogTailBoundaries/post-scrub_clamp`（经真实 `safelog.SetupLogger` 输出的组合验证）、`TestWatchDaemonExitCleanExitStaysQuiet`/`TestWatchDaemonExitWarnSuppressedByLiveHolder`（直接驱动真实 watcher + done 握手取代固定 sleep）、`TestSpawnDetachedLogTailBoundToOpenedFile`（ready→换链→release FIFO 握手）、`TestSpawnDetachedDaemonLogOpenFailureFailsClosed`（合并 EISDIR 重复子项并补 errno/root/logPath/启动准备计数断言）。
- **`WorkdirAllowlist` 因无关旋钮拼写错误退化扩大授权边界（安全）**：权限根与其余配置键不是同一风险级别，但旧版一律反序列化完整 `yamlConfig`——`workdirs: [/root/workspace]` 搭配 `nice: invalid`（或 `max_procs` / `index_workers` / `cpu_quota_percent` 任一个非法值）会让整份 `yaml.Unmarshal` 报错，权限根被丢弃并退回 `$HOME=/root`，把「只授权 workspace」悄悄放宽成「整份 home 可索引」。改为专用最小结构 `workdirsOnlyConfig`（只读 `workdirs` 一键）：非权限键的错值只让资源旋钮失效（`LoadConfig` 照旧日志并忽略该文件），不再抬高权限根；YAML 语法本身损坏（如 `workdirs: [unterminated`）的既有契约（退回 `$HOME`）不变。回归测试 `TestWorkdirAllowlist_InvalidResourceFieldPreservesWorkdirs`（8 种非法资源值 + 语法损坏对照组；修前 `allowlist = [/root]` FAIL，修后 PASS）。
- **`exclude_dirs` 与 rg 的 glob 引擎语义分裂**：索引遍历用「字面相等 + `filepath.Match`」，`rgExcludeArgs` 却把原始串直接交给 rg 的 gitignore globset，两处解释不一致：`{foo,bar}` 在 walker 是一个字面目录名，rg 却展开成 `foo/`、`bar/` 两个分支（丢了索引仍会走的子树）；字面目录 `data[` 在 walker 能排除，rg 却直接 `error parsing glob '!data[/': unclosed character class` 退出 2，搜索/文件列表整个失败。新增单一转换函数 `extraction.RgExcludeGlob`：沿用 walker 的锚定/裸名分类，转义花括号（两边解释唯一分歧的构造），对 `filepath.Match` 就拒收的模式（未闭合 `[`、非法 range、悬空转义）按纯字面目录名转义全部通配元字符，并保证拼接 `!\<glob>/` 后不会出现令 rg 报错的「偶数结尾反斜杠」（实测 ripgrep 14.1.1：奇数结尾合法、偶数即 exit 2）。现任何 `exclude_dirs` 配置都不会令 rg 报错，且 rg 与索引器排除结果一致。后续复审在该转换层再抓出三类词法级误译并已根治：(a) `filepath.Match` 拒收的模式按纯字面量转换时漏转义 `\\` 本身（`foo\\[bar` 仍是 unclosed character class → exit 2）；(b) 合法 Go 模式的逐 token 翻译——`\\c` 转义对逐字节插 `\\` 会把 `\\{foo` 变成 `\\\\{foo`（literal backslash + unclosed alternate group → exit 2），现按 Go 词法消费双字节，`\\{`/`\\}` 译为类形式 `[{]`/`[}]`（实测 rg 花括号展开在转义解析之前，`\\{` 仍计作 alternation opener），`\\` 译为 `[\\]`（裸 `\\` 在 `!\\<glob>/` 尾部恒为 dangling '\\' exit 2，连 fixRgTrailingEscape 都无法补救），`**` 折叠为 `*`（Go 无 globstar，`a/**/b` 仅匹配单层），`[^a]`→`[!a]`（Go 的否定 `^` 译为 rg 的 `!`）；(b-续) 第 4 轮复审按 ripgrep 14.1.1 的 POSIX-ish 类词法重写类归一化（`parseGoClass` + `normalizeRgRangeEndpoints` + `emitRgClass`）：rg 类内 `\` 为字面量而非转义，故 `[!a]`→`[a!]`（`!` 避开首位）、`[!]`→`\!`、`[!^]`→`{\!,\^}`、`[a\]b]`→`[]ab]`（`]` 置首）；反向区间 `[z-a]` 按 Go 空集丢弃（rg 会 exit 2），空否定类 `[^z-a]` 化为 `?`；区间端点上的 `]`/`-`/`!`/`^` 剥入 singles；类内 `{}`/`,` 为字面量直传不转义；`a\` 类悬空转义经 `a[\\]` 字面类精确排除字面目录。(c) walker「字面相等 OR `filepath.Match`」双语义需要双 glob——`\\{foo`、`[ab]` 这类合法模式的字面目录名不命中自身模式却会被 `name == pat` 排除，新增 `extraction.RgExcludeGlobs` 同时返回通配翻译与纯字面量翻译（去重），`rgExcludeArgs` 遍历发射；`""`/`"."`/`".."` 规则直接早退（保留合法目录名 `"..foo"`）。第 5 轮复审再根治三项：(d) `filepath.Match(pat, "")` 在首个含非空前缀的 `*` 分片失配空串后直接短路返回 `(false, nil)`，从不扫描 `*` 之后的分片——`foo*\`、`foo*[`、`foo*[a-` 这类「` *` 后缀非法语法」规则被误判为合法 Go 模式进入逐 token 翻译，`translateRgGlob` 在 `case '\\':` 处对 `pat[i+1]` 越界 panic；新增 `isValidGoPattern`（按 `scanChunk` 词法逐 `*` 分片调 `filepath.Match(chunk, "")`，任一分片报错即拒绝）替换 `RgExcludeGlobs` 与 `rgSafeGlob` 两处裸 `filepath.Match(pat, "")`，并在 `translateRgGlob` 加 `i+1 >= len(pat)` 防御边界；(e) 字符类对 `'/'` 的处理在裸名规则与路径规则两侧与 walker 分叉——`ShouldSkipDirIn` 裸名分支只拿 `filepath.Base(path)`（绝不含 `'/'`）比对，但 rg 的无锚定目录 glob 经 `globset` 展开为 `**/<glob>`，其中否定类 `[!x]` 与跨 `'/'` 区间 `[.-0-]` 都会命中分隔符把两层目录 `cmd/codegraph-go` 误排除；修复为按 `allowSlash`（`anchored || strings.Contains(pat, "/")`）分流——裸名规则下正向区间在 `'/'` 处拆分（`[\--0]` → `[.0-]`）、非空否定类把 `'/'` 补入成员集（`[!x]` → `[!x/]`）并因 glob 含 `'/'` 而自动加 `**/` 前缀保持任意深度生效，路径规则（含 `'/'` 或 `./` 锚定）保持 `'/'` 合法成员不拆（Go `getEsc` 不禁止 `'/'`，`filepath.Match("a[/]b","a/b")` 实测为 true；rg 14.1.1 `a[/]b`/`a[a/]b` 亦仅排除 `a/b`），`parseGoClass` 删去错误的 `r == '/'` 过滤；(f) `deploy.sh` 备份阶段 `protect_binary=1` 从 `then`/`else` 内部提前到 `cp -pf`/`install` 之前，`cp`/`install` 中途收到 `SIGINT`/`SIGTERM` 时 `relock_binary` 同样兜底清理半截 `.old`/`.new` 并保证 `$BINARY` 为 `0555 + i`。第 5b 轮原件复核再收口三处残余：段间空否定类 `[^z-a]` 在 `allowSlash` 路径规则中可合法落在 `'/'` 上（`filepath.Match("par[^z-a]b","par/b")` 为 true）而 rg 的 `?` 拒绝分隔符——非邻接 `'/'` 且非首尾的空否定类改发射 `{?,[/]}`（实测 rg 14.1.1 `!/par{?,[/]}b/` 精确排除 `par/b` 与 `parYb`、`parxb` 不误伤）；多斜杠锚定 `///`/`.///` 与尾随斜杠串经 `TrimLeft`/`TrimRight` 归一为空早退，不再向 rg 发射 `"//"`（实测 rg 14.1.1 `!//a/` 恒不匹配）；`[/]`/`./[/]` 经双 glob 发射（通配 `[/]` + 字面 `\[/\]`），实测 rg 14.1.1 `!/[/]/` 零报错、与 walker 一致。第 6 轮复审再收口三项：(g) `ShouldSkipDirIn` 与 `RgExcludeGlobs` 的归一化拆成两份后已漂移——r5b 在 rg 侧用 `TrimLeft`/`TrimRight` 剥重复 `'/'`，walker 侧仍用单次 `TrimSuffix`/`TrimPrefix`，`"a//"`、`"./a//"`、`".//a"`、`"//a"` 在 walker 侧永不命中而 rg 侧已排除；现抽出共享 `normalizeExcludeRule`（同一 `ToSlash(TrimSpace)` → 锚定判定 → 剥前导 → 坍缩尾随 → 空早退），walker 裸名/路径分支条件与 `allowSlash` 完全同构，两侧 100% 一致（`skip_test.go` 尾段 + `rg_exclude_test.go` 新增 `trailb//`/`./anchc//`/`.//anchd`/`//anche//`/`./.`/`./..` 规则与候选目录，真实 `rg --files` 逐目录比对）；(h) 显式记录 ripgrep 14.1.1 词法验证基线于 README 与 `.agents/notes/20261101-resource-limits-and-rollback-notes.md`——升级本机 `ripgrep` 后应运行 `go test -p 2 ./internal/extraction ./internal/server -run TestRgExcludeArgsWeirdRulesAgreeWithWalker` 校验兼容性；(i) `deploy.sh` 替换分支 `protect_binary=1` 再前置至 `if [ -f "$BINARY" ]` 内第一行，预清理残留 `.old` 期间收到外部信号同样走 `relock_binary` 兜底。新增 `TestRgExcludeGlobTranslation`（81 例形状/转义表）、`TestRgExcludeGlobStarSuffixBadSyntax`（`*` 后缀非法语法不 panic、纯字面翻译、rg 14.1.1 零报错）、`TestExcludeDirsSlashClassBareVsPath`（裸名/路径 `'/'` 分流逐目录验证）、`TestExcludeDirsGlobMetaRulesAreLiteral`（walker 侧字面语义）与 `TestRgExcludeArgsWeirdRulesAgreeWithWalker`（真实 `rg --files` 逐目录比对 walker 判定；修前 `data[` 直接 exit 2 FAIL）。
- **systemd-run 同步回退未清 `quotaArgs`，守护进程被拉两次**：`cmd.Start()` 同步失败（如 systemd-run 不可执行）会回退到直接 `exec.Command(self, args...)`，但 `quotaArgs` 仍非空，于是直接启动的子进程也走了下面 `if quotaArgs != nil` 的 150ms 异步探测窗——而该分支的第二个条件是「pidfile 尚无存活守护进程」，一个在窗内非零退出的直接启动子进程（启动失败）会被当成 scope 创建失败，再拉起一次。现在回退前显式 `quotaArgs = nil`。回归测试 `TestSpawnDetachedSystemdRunSyncFallbackClearsQuota` 用会记录每次 exec 的假 daemon 脚本计数拉起次数并校验父 PID（修前 2 次 FAIL，修后 1 次 PASS）。
- **`deploy.sh` 预热绕过 `CPUQuota` 第四层硬配额**：部属预热用 `CODEGRAPH_DAEMON_INTERNAL=1 setsid "$BINARY"` 直接起 daemon，那是与 Go 侧 `SpawnDetached` 平行的第二套启动入口，绕过 `systemd-run --scope -p CPUQuota=<N>%` 及其两级回退、workdir 收窄与权限校验——同一份配置下，客户端拉起的 daemon 有硬配额、部属预热出的 daemon 没有，上限形同虚设；就绪探测还得拿子 shell 的 `$!` 与 pidfile PID 相等作条件（systemd-run 包装后恒不可能相等）。改为调 `"$BINARY" -workdir "$WORKDIR" [-config "$DEPLOY_CFG"] -warm-daemon`（只剩一套 daemon 启动实现），就绪探测保留并改为以 pidfile 记录的**真实 daemon PID** 为准（pidfile + socket + flock 持有 + `daemon_matches` 全套守护进程特征复校），不再与客户端命令的 PID 比对。本机实测：`-warm-daemon` 拉起的 daemon 位于 `system.slice/run-*.scope`，cgroup `cpu.max = 150000 100000`（= 1.5 核）。
- **`deploy.sh` 二进制不可变锁（0555 + `chattr +i`）**：旧版 `install -m 755` 留下的生产二进制对同用户可写，`cat > ~/.local/bin/codegraph-go` 一类误写会悄悄污染它。现在 staged 新二进制一律 `install -m 0555`，替换改为锁内单次原子 `mv`：`cp -pf` 先备份 `$BINARY.old` + `install -m 0555` 预置 `$BINARY.new`（全程 `$BINARY` 在位且未解锁）→ 置位 `protect_binary=1` → `unlock_binary` → 单次 `mv -f "$BINARY.new" "$BINARY"`（POSIX rename 单步覆盖，无双 `mv` 缺失窗）→ `chattr +i`；解锁后中断时 EXIT trap（`protect_binary=1` 已置位）自动重锁并清理 `.old`/`.new`，绝不遗留残缺或遗留文件。`CODEGRAPH_SKIP_IMMUTABLE_LOCK=1`（测试环境，避开 /tmp 只读文件清理 EPERM）、非 root 或无 `chattr`、文件系统不支持不可变位（btrfs/overlayfs/tmpfs）三种情形逐级降级为纯 0555 并记日志，不算部署失败。新增 `scripts/test_deploy_immutable_lock.sh`（从 `deploy.sh` 原样抽取「替换二进制」段执行，不重抄逻辑；24 项断言：静态 6 项 + 首装 2 项 + 替旧 4 项 + 两处注入崩溃后 trap 恢复 8 项（含解锁窗崩溃独立夹具与 chattr +i 真实恢复）+ install 失败重锁 4 项）。
- **Pi 扩展漏同步（部署副本落后仓库一个提交）**：deploy.sh 此前只替换 Go 二进制，Pi 扩展靠 `integrations/pi/README.md` 手工 install——在跑的旧版缺 `wantsKeepCode` 逃生口，会把 `action=node` / `includeCode=true` / `skipCode=false` 返回的代码围栏与 `file:line:content` 二次剥掉，源码正文丢失。新增 `integrations/pi/install.sh`（`install -m 644` 到 `~/.pi/agent/extensions/codegraph-go.ts`，md5 一致即跳过、不重写）并由 deploy.sh 调用，打印源/目标 md5 与是否变化；Pi 侧需 `/reload` 或新会话生效。同步失败不阻断 daemon 部署，但在 deploy.sh 结尾 WARN 汇总重打；install.sh 对 md5sum 缺失 / HOME 未设置 / 目标为目录 / md5 取空一律显式 `FAILED` 并非零退出，不再有静默失败路径。根因与后续项见 `.agents/notes/20261003-pi-ext-deploy-sync.md`。
- **`integrations/pi/install.sh` 覆盖变量与 HOME 校验顺序倒挂**：修前先校验 `HOME` 再取 `PI_EXT_DEST`，导致显式设了 `PI_EXT_DEST` 也照样被「HOME is not set」拦下，而报错文案偏又教用户「set PI_EXT_DEST explicitly」，文案与行为自相矛盾（实测 `env -u HOME PI_EXT_DEST=/tmp/x.ts bash install.sh` 修复前 FAILED、exit 1）。改为优先采纳 `PI_EXT_DEST`、仅在未指定时才校验 `HOME` 推导默认路径；顺带把目标为目录的报错文案由 `(dirname 而非目录本体)` 统一为 `(须指定文件名而非目录本体)`（前者在 shell 语境下易被读成「父目录」）。仅此两处改动，其余契约（`command -v md5sum` 预检、`md5_of` 不吞 stderr/退出码、源或目标 md5 取空即败、DEST 必须绝对路径、md5 幂等不重写不动 mtime、结尾 `/reload` 提示）不变。四处 `install.sh` 契约副本同款，本修复对齐权威口径，详见 `.agents/notes/20261003-pi-ext-deploy-sync.md`。
- **`deploy.sh` 前置分支/工作区洁净度门禁与 `integrations/pi/install.sh` 尾部斜杠拦截硬化**：`deploy.sh` 在编译前新增 `main` 分支与工作区洁净度（`DIRTY_STATUS="$(git status --porcelain)"` 独立赋值防 `if` 吞返回码）前置门禁，防止在非 `main` 分支或含未审改动的工作区误将代码盖入生产二进制与 Pi 扩展目录（支持 `DEPLOY_ALLOW_DIRTY=1` 豁免分支与脏树检查，或在 `main` 分支设 `DEPLOY_COMMIT=1` 自动豁免脏树检查以配合部署后自动提交）；`integrations/pi/install.sh` 在 `-d "$DEST"` 之前显式拦截 `*/` 尾部斜杠路径（统一输出 `install.sh FAILED: PI_EXT_DEST ($DEST) must not end with '/' ...` 并 `exit 1`），且为 `mkdir -p "$DEST_DIR"` 与 `install -m 644 "$SRC" "$DEST"` 两处追加 `|| fail "..."` 兜底以统一 `FAILED:` 报错前缀。详见 `.agents/notes/20261003-pi-ext-deploy-sync.md`。
- **`integrations/pi/install.sh` 原生 Go 扩展共存保护（zero-node 宿主机致命回归）**：宿主机移除 Node.js 并将 `~/.pig/agent/extensions/*` 迁移为指向 `pi-extensions/go/*` 的原生 Go 扩展目录软链后，`install.sh` 仍无条件向默认路径 `$HOME/.pig/agent/extensions/codegraph-go.ts` 写入 TypeScript 副本——同名 `.ts` 落进扩展目录会让 pig 启动时调起已不存在的 `node`（>=22.13）重打包，报 `spawn:runtime_unavailable` 致全机 pig 会话无法启动。修复：目标解析时记录 `EXPLICIT_DEST`，仅在走默认路径（`EXPLICIT_DEST=0`）且 `${DEST%.ts}` 为含 `go.mod` 的目录（`-d` 穿透软链）时，先自愈清除残留 `.ts`（`rm -f`，失败显式 FAILED），再输出 `skipped: native Go extension active at $NATIVE_EXT ...` 并 `exit 0` 跳过安装；显式 `PI_EXT_DEST` 或无原生扩展时原安装与 md5 幂等行为 100% 不变。`scripts/test_deploy_immutable_lock.sh` 新增场景 5（总计 29/29 断言）以临时 HOME 隔离回归验证，不触碰真实 `/root/.pig`。详见 `.agents/notes/20261003-pi-ext-deploy-sync.md`。
- **`halfDeadDaemonHoldsLock` 与 `newbornDaemonInStartupGrace` 的 pidfile 诊断读取共享同一叶安全读面（O1 续）**：两个探针此前各自用裸 `os.ReadFile(PidPath(root))` 按路径读 pidfile——`.codegraph/daemon.pid` 若为无写端 FIFO/设备，open(2) 永久阻塞：`halfDeadDaemonHoldsLock` 卡住 spawn 判定与 direct-mode 回退决策、`newbornDaemonInStartupGrace` 卡死 invisible-holder 清扫；若为叶软链则跟链读项目外文件，一份指向活进程的伪造 LockInfo 即可让 `halfDeadDaemonHoldsLock` 误将健康守护进程分类为 half-dead 而进入杀进程流程、或让 `newbornDaemonInStartupGrace` 误授新生宽限而跳过本轮 holder 清扫。两个探针改与 `daemonAliveAt` 共用 `readRegularPidfile`：单次 `unix.Open(pidPath, O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC)` 在 open() 对叶软链返回 ELOOP（不跟链出项目）、对 FIFO/设备叶立即返回，随后在同一已绑定 fd 上 `f.Stat` 复核普通文件再 `io.ReadAll`——无 Lstat→ReadFile 换绑窗口；打开失败/非普通叶/读错一律降级为「无 pidfile」（half-dead 判否、宽限不授），不改 `IsProcessAlive`/`StartedAt`/宽限窗口判定，也不动 `TryAcquireLock`/`ClearStaleLock`/`KillStaleDaemon`/`checkDirectFallbackSafe`/`lockHolderHint`/deploy.sh 等既有 pidfile 读点（O_NOFOLLOW 仅约束最终叶、不防父目录软链，`io.ReadAll` 无独立尺寸上限——见 `.agents/notes/20261110-pidfile-probes-safe-read.md` 登记的观察项）。回归：`TestPidfileProbesRejectSymlinkLeaf`（按探针拆分的两个 t.Run：伪造目标分别以「超过宽限期的 StartedAt」与「新鲜 StartedAt」命中各自探针的判定分支）、`TestPidfileProbesFifoLeafDoesNotBlock`（两个独立 root/FIFO 子测试，均无写端 FIFO 下有界返回 false）。

### Changed
- **L2 基线收敛（检查入口统一为 `make check`）**：新增根 `Makefile`，`check` 目标依次执行 `gofmt -l`（非空即败）、`CGO_ENABLED=1 go vet ./...`、`CGO_ENABLED=1 go test -race ./...`、`CGO_ENABLED=1 go build -o /dev/null ./cmd/codegraph-go` 与 `staticcheck ./...`；`.github/workflows/ci.yml` 将原本分散的 gofmt/vet/test/build/staticcheck 执行步骤收敛为单一 `make check` 步骤（静态检查工具仍由 CI 显式 `go install`）；`AGENTS.md` 声明 `档位：L2` 并将提交规范改为指向全局规则文件，不再复述全局提交/收口规则字面内容。复核收口两处加固：`gofmt -l` 捕获失败时显式传播其非零退出码（修复 Make `/bin/sh` 无 `-e` 时吞掉非当前平台文件语法错的问题），并在 `check` 首步快检 `staticcheck` 是否就位（缺失时立即报错并提示 `go install honnef.co/go/tools/cmd/staticcheck@2026.1`，不再白跑完整测试后才失败）；`.gitignore` 的 `AGENTS.md` 反忽略规则收紧为根锚定 `!/AGENTS.md`（仅解除仓库根同名文件的全局忽略，不影响子目录）。
- **`codegraph-go init <root>` 漏做 `EvalSymlinks` 规范化**：`runInit` 把未规范化的 `root` 交给 `ValidateWorkdirs`、把 `filepath.Abs` 的结果交给 `db.Open`，而 `main()` 对每个 workdir 都做 Abs + EvalSymlinks；`<root>` 是符号链接时，init 会在链接位置多建一份 `.codegraph`，与后续会话用的规范路径不是同一个索引库。现与 `main()` 同调 `canonicalize(abs)`，同一份规范化结果同时给 `ValidateWorkdirs` 与 `db.Open`。
- **守护进程在 workdir 被删后空占锁**：派发会话常把 workdir 收窄到 `.wt/<repo>/<branch>` worktree，agent 干完活删掉 worktree 后，守护进程仍持有 DB flock、pidfile 与 socket 直到（默认 5 分钟）空闲超时，这段时间同一位置重建目录也会被旧守护进程占着。现在后台每 15s `os.Stat(d.root)` 一次，目录已不存在且**无活跃会话**时主动 `Stop("workdir removed")` 释放锁与句柄（有会话在跑就不退，绝不打断进行中的工具调用）。该监控 goroutine 故意不加入 `d.wg`——它自己会调 `Stop`，而 `Stop` 要等 `d.wg`，加进去就死锁。新增 `TestDaemonExitsWhenWorkdirRemoved`（覆两分支：无会话自动退、有会话等会话完再退）。
- **守护进程「无会话即退」与握手/登记的竞态窗口**：旧版 `watchRootLiveness` 与 `armIdleTimer` 的「先解锁读 `ClientCount()==0`、再 `Stop`」与 `acceptLoop`/`serveConn`/`addClient` 之间存在两个窗口——已 `Accept()` 未完成 ClientHello 的连接对 `ClientCount` 不可见（停机可把 listener 从握手中的连接下抽走），且 `stopping` 置位后 `addClient` 仍会登记新会话。现在 `Daemon` 增加 `mu` 保护的在途连接计数 `inFlight`：`acceptLoop` 在 `Accept` 成功后同一把锁内查 `stopping`、`inFlight++` 并停 idleTimer，`serveConn` 以 `defer finishConn()` 递减（`clients==0 && inFlight==0` 时重新排 idle 定时器）；`addClient` 改为返回 `bool`，持锁见 `stopping` 即拒绝登记、`serveConn` 收到 `false` 立刻退回不启动 handler；新增原子方法 `stopIfIdle(reason)`，在同一把 `d.mu` 内完成「`clients==0 && inFlight==0` 检查 + `stopping` CAS + 摘 listener」，出锁后关 listener 并走与 `Stop` 共用的 `finishShutdown` 收尾（`close(d.stopped)` 只执行一次）。`watchRootLiveness` 与 idle `AfterFunc` 均改调 `stopIfIdle`，忙时分别 `continue`/重新排下一拍。新增 `TestDaemonStopIfIdleDoesNotCutInFlightOrHandshakingSession` 回归（握手期 parked 连接 + 并发会话下拒绝停机，排空后干净退出且 listener 拒新连接）。
- **Linux `setNice` 的「两轮扫描」改为有界不动点**：固定 2 轮既没有依据也不能写成一个保证。现在最多 8 轮重读 `/proc/self/task`，每轮只处理未见过的 tid，连续一轮无新 tid 即收敛退出（上限存在只为防无限克隆线程的负载把启动循环变成无限旋转）。函数注释与 `README.md` / `.agents/notes/20261101-resource-limits-and-rollback-notes.md` 同步改写为「有界多轮收敛 + 极端并发 CGo 线程派生下 best-effort」，并写明硬上限属于 `cpu_quota_percent`（cgroup）这一层，不再过度承诺。
- 统一 scripts/notes-index.sh 索引脚本，并在 AGENTS.md 补充索引刷新指引。
- AGENTS.md 补充全局 commit-msg hook 提交规范。
- `integrations/pi/codegraph-go.ts` 注释中的过时仓库路径回灌修正（`/root/codegraph-go` → `/root/workspace/codegraph-go`，仅注释，无逻辑改动）。
- `AGENTS.md`（本地忽略文件）「关联仓库」章节移除已归档退役的 `prism` 条目（详见 `.agents/notes/20261016-drop-prism-related-repo.md`）。

## [0.9.12] - 2026-09-13

### Fixed
- **未解析引用重试上限**：`unresolved_refs` 新增 `attempts` 列（addMissingColumns 增量迁移，不触发索引重建），解析失败累计 20 轮后置为 `abandoned`（保留审计行、退出重试池）——终结标准库/内建调用积压（生产实测每轮 ~1.4 万次注定失败的重复解析）。
- **增量索引的 inbound refs parking 收进单事务**（全程持 d.mu）：消除与并发 `ReplaceFileIndex` 级联删除竞争时的 FOREIGN KEY 失败窗口（原 M7 容忍路径关闭），park 失败整体回滚不留半截（触发器注入 + -race 压测验证）。
- 解析统计行扩为 `resolved N edges (failed, retried, abandoned)`，三类计数语义见代码注释。

### Changed
- Display / daemon wire version **0.9.12**。

## [0.9.11] - 2026-09-12

### Changed
- 删除 internal/sync 的死代码 `Watcher.AddDir`/`RemoveDir`（全仓零调用，第五轮全面审计唯一可操作项）。
- `cgdir.Ensure` 返回验证过的目录身份（Dev/Ino），daemon Start 直接采用——消除 Ensure 与独立 stat 之间的微秒级换手间隙（第五轮对抗审计 F6，30 次竞态未命中的理论窗口）；新增 `internal/cgdir/identity_unix.go`/`identity_other.go` 平台拆分。
- `CODEGRAPH_ALLOW_PLAIN_DB=1` 降级模式自声明：告警中明确"plain-path DSN 下目录换手由 recheck 检出而非阻止"（第五轮对抗审计 F1）。
- Display / daemon wire version **0.9.11**。

## [0.9.10] - 2026-09-12

### Fixed
- **无 procfs 环境 fail closed（第四轮对抗审计实证的条件性残留）**：linux 上 `/proc/self/fd` 不可用时 `db.Open` 默认拒绝启动并给出可操作错误，不再静默回退到仅检测的 plain path（unshare+tmpfs 遮 /proc 下 5/5 越界后检出的路径）；`CODEGRAPH_ALLOW_PLAIN_DB=1` 作为逃生门保留降级并打告警。
- `RunAsDaemon` Start 失败路径的 pidfile 删除改走 artifact 守卫（dirID 复核），与 SIGTERM 清理同纪律——换手后不再经路径删除界外文件。
- 运行期新增目录的 watch 失败统一计入 `watcher_blind_dirs`（此前 watchTree 直连 Add 不计数，status 低估盲区）。
- extra workdir 的 watcher 状态可观测：启动失败按根记录并在 status 输出 `watcher_active: false for workdir ...`，pending/dropped/blind/unreadable 计数对 extra roots 同样输出。
- pidfile 新增可选 `dirDev`/`dirIno` 字段（omitempty 向后兼容）+ daemon 启动日志记录 `.codegraph` 目录身份——同 inode 目录搬移留下审计线索。

### Changed
- pin_other.go 注明非 unix（含 windows）平台整仓构建不受支持（tree-sitter cgo 上游限制，0.9.5 前即如此）。
- Display / daemon wire version **0.9.10**。

## [0.9.9] - 2026-09-12

### Fixed
- **TOCTOU 根治（第三轮对抗审计 20/20 实证的检测型残留）**：`db.Open` 改经 `/proc/self/fd/<dirfd>/codegraph.db`（procfs magic link 恒定解析到钉住目录 inode）打开 SQLite——db/-wal/-shm 全部不再经路径重解析，目录换手后写入仍落在钉住的真实目录；pinned fd 由 DB 持有至 `Close`（openat 加 `O_CLOEXEC` 防泄漏）；非 Linux 或 procfs 不可用回退路径打开并保留 inode 复核。实测换手后写入零越界（修复前 20/20 越界可复现）。
- **失败不再静默**：safelog 暴露 `Flush` 并在全部致命退出路径排空缓冲——`main.go` 3 处 `log.Fatalf` 与 9 处 `slog.Error` 后的 `os.Exit(1)`（allowlist 拒绝、symlink 拒绝、TOCTOU 检出、flock 冲突此前全部 0 字节 stderr，5/5 复现）修复后 stderr 均含可操作错误；`Close` 改为先 Flush 再关，顺带修复 PPID watchdog 日志行丢失（正常返回路径进程退出过快所致）。
- **Stop 清理身份复核**：daemon 记录 `.codegraph` 目录 dev/ino，`cleanupArtifacts` 删除 pidfile/socket 前复核——目录被换手后不再经 symlink 删除界外文件（红队实测的第三条删除通道：Go `UnixListener` unlink-on-close 亦已 `SetUnlinkOnClose(false)` 收口到守卫点）。
- **pidfd 首信号前补身份复核**：`terminateStaleViaPidfd` 在 pidfdOpen 后重跑 `verifyDaemonIdentity`（镜像 recovery 侧模式），封死 verify→pidfdOpen 的换进程窗口；失配且进程已死则清锁，存活则拒绝。
- **watcher 自愈**：Start 失败按退避重试（5 次 1s-16s，关机即时中止），最终失败在 status 可见（`watcher_active: false`）；盲区计数（Add 失败/walk 读失败）升为原子字段并进 status（`watcher_blind_dirs`/`watcher_unreadable_dirs`），不再"启动日志一次即失忆"。
- `RegistryDir` 在 $HOME 不可解析时放弃注册（best-effort），不再写 `/tmp/.codegraph/daemons`（walk-up 误认污染源）。

### Changed
- 修正 `BoundedEditDistance` 注释（实为普通 Levenshtein，无换位步）；deploy.sh 在 lock 探测前拒绝 symlink 的 `.codegraph`。
- Display / daemon wire version **0.9.9**。

## [0.9.8] - 2026-09-12

### Fixed
- 修复 CI flake（GH Actions 上 `TestVerifyDaemonIdentitySameProjectDaemon` 偶发失败）：`/proc/<pid>/stat` 的 starttime 在 fork 时固定并延续到 exec 之后，而 `/proc/<pid>/cmdline` 在 fork→exec 窗口内显示的是父进程 argv——慢速 runner 上测试 helper 刚启动即被读取会看不到 `-workdir` 而误判拒绝。`startDaemonLikeProcess` 现在等待目标 argv 真正可见后再返回（2s 兜底）。
- proxy 在 host stdin EOF 时对 unix socket 做 `CloseWrite` 半关闭而非整体断开，并等待 daemon 侧输出排空（5s 兜底）再收尾——修复"先关 stdin、再读 stdout 收尾"顺序下最后一条 JSON-RPC 响应被截断。
- watcher 对非根目录的 inotify Add 失败降级为计数+日志并继续遍历，仅根目录失败才中止启动——超大仓库撞 `fs.inotify.max_user_watches` 时不再整体退出。
- files 表时间列统一为 Unix milliseconds：`indexed_at` 此前写秒（只写不读，纯防雷），schema 注明两列单位。
- db 包导出 `ErrIndexInUse` sentinel 并在 flock 冲突源头包装（渲染文案逐字节不变），`main.go` 改用 `errors.Is` 判定，保留驱动层 `locked` 文案回退。
- `callTargetKinds` 双份实现增加跨包一致性测试护栏（internal/db 与 internal/resolution 快照深比较）。

### Changed
- README 新增 "Shared daemon socket auth" 一节：`CODEGRAPH_MCP_TOKEN` 的作用、启用方式（双侧同值）、默认关闭与拒绝行为；明确 0600 socket 权限仍是第一道防线。
- 删除 extraction/orchestrator.go 中 3 处 0.9.5 拆分遗留的悬空 doc comment（fileNodeCount/IndexFile/indexFile）。
- deploy.sh 的 DEPLOY_COMMIT 提交集合补入 CHANGELOG.md 与 README.md。
- Display / daemon wire version **0.9.8**。

## [0.9.7] - 2026-09-12

### Fixed
- `.codegraph` 防线的 TOCTOU（第二轮对抗审计实证击穿）：`db.Open` 校验通过后立即以 `openat` 钉扎目录 fd（`O_NOFOLLOW|O_DIRECTORY`），锁文件经 `openat(fd)` 创建，flock 后与建库后各做一次 Dev/Ino 复核；残余窗口压缩到 syscall 边界（modernc SQLite 仅收路径，无法归零，已在代码注释如实注明）。非 unix 平台退化为路径级校验。
- daemon 侧三个 `.codegraph` 写点（spawn 的 daemon.log、TryAcquireLock 的 pidfile、Start 的 socket bind）此前完全绕过 symlink 防线：防线抽为共享包 `internal/cgdir`（`Ensure` = Lstat 拒绝 → MkdirAll → realpath 校验），四处接入，界外文件零落盘。
- stale daemon 击杀身份绑定项目：`verifyDaemonIdentity` 在 procStart 匹配后追加要求目标进程 cmdline 的 `-workdir` 等于本项目 root（与 isInvisibleHolder 同口径），environ 单项 / exe 单项 / 跨项目伪造 pidfile 均不再放行；升级清理路径（同项目真 daemon）端到端实测不回归。
- `KillStaleDaemon` 信号段改走 pidfd（复用 recovery 侧基建，pidfd 不可用时回退 kill(2)+recheck），封死 recheck→signal 的 PID 复用窗口。
- 修正 recovery.go 中"为避免 import cycle 而复制 flock 探测"的不实注释（db 是叶子包，无环）。

### Changed
- Display / daemon wire version **0.9.7**。

## [0.9.6] - 2026-09-12

### Fixed
- `.codegraph` 本身为符号链接时索引数据库会被写到 workdir 之外（对抗审计实证）：walk-up 判定改用 `Lstat`，`db.Open` 增加 Lstat + realpath 双重防线，symlink 形态的 `.codegraph` 拒绝打开并给出可操作错误。
- stale daemon 清理的身份校验可被伪造 pidfile 绕过：击杀条件改为 pidfile procStart 匹配 且 `/proc/<pid>/exe` 等于本二进制 或 environ 含 `CODEGRAPH_DAEMON_INTERNAL=1`；legacy 无 procStart 的 pidfile 不再进入击杀路径（fail closed），升级清理路径用真实旧 daemon 实测保持可用。
- `store_fact` / `search_facts` 声明的 30 秒超时实际从未生效：facts 查询补齐 Context 变体并全链路透传。

### Changed
- `ResolveAll` / `ResolveForFiles` 的边写入改为按 500 条一批的单事务提交，批失败回退逐条隔离坏行；大仓库 resolve 阶段的事务提交数从 ~2N 降为 N/500。
- CI 新增 `go test -race`、`gofmt`、staticcheck（pin 2026.1）三道严格门禁。
- 清理 0.9.5 拆分残留：15 个文件 526 行重复文档注释、5 个测试文件 gofmt、staticcheck 10 条告警清零；删除过期的未跟踪 coverage.out。
- README 对齐实际能力：移除不存在的 comparison.md 引用、架构树更新为拆分后的真实文件、tree-sitter 覆盖更正为 15 种语言、框架路由清单以 `frameworks.go` 实际为准（补 GoFrame、React Router）。
- Display / daemon wire version **0.9.6**。

## [0.9.5] - 2026-08-21

### Changed
- 纯重构，无行为变化：拆分三个千行文件。`db/query.go`（2493 行）按领域拆为 nodes/files/refs/edges/facts/index 六个文件；`extraction/orchestrator.go`（1308 行）拆出文件级操作与 `indexFile` 写入器；`extraction/extractor.go`（1443 行）按语言族拆为六个文件。全量测试与 race 验证通过。
- Display / daemon wire version **0.9.5**。

## [0.9.4] - 2026-08-21

### Fixed
- callers/impact 的 rg 回退加 `--` 分隔符：`-` 开头的符号名按字面匹配，不再被解析为 rg flag（参数注入，可改变搜索行为）。
- synthesize 按文件读节点改用带截断报告的查询，超过单文件上限时打日志；`ResolveForFiles` 的 changedNames 改为全量分页遍历，超上限文件不再丢候选。

### Changed
- 文件候选 LIKE 查询上限从 1000 提高到 10000。
- deploy.sh 安装二进制移到任何 kill 之前：kill 窗口内客户端复活的 daemon 必为新版本；安装失败时先于停进程中止，不再产生「旧 daemon 已死、新二进制未就位」的空窗。
- `findGoModule`/`findNPMPackage` 的跨界上溯补充豁免说明（monorepo 场景故意越过工作区边界读模块名）。
- Display / daemon wire version **0.9.4**。

## [0.9.3] - 2026-08-21

### Fixed
- daemon unix socket 绑定时临时设置 umask 0077，堵住 socket 创建到 chmod 之间的组/其他权限窗口；最终权限仍为 0600。
- 同名候选超过 1 万条时不再被静默丢弃：`CollectCandidates` 改为分页遍历全部匹配。
- deploy.sh 预热就绪判断要求 pidfile 记录的 pid 等于本次 spawn 的进程，不再把抢先复活的另一个 daemon 的产物误判为新 daemon 就绪；spawn 失败但已有存活 daemon 在服务时改报 INFO 而非 WARN。
- deploy.sh 注册表清理在有存活 daemon 时跳过按 workdir 的记录删除，只按被杀 PID 清理，存活 daemon 的发现记录不再丢失。

### Changed
- deploy.sh 的 daemon 注册表清理改用 `scripts/cleanup_daemon_registry.py`（JSON 解析后精确匹配 PID/root，畸形记录保留，出错立即失败）；deploy.sh 现在依赖 python3，缺失时部署直接失败并报错。
- 按文件、按名的节点查询增加 1 万条上限与 keyset 分页；图快照改用不含 body 的轻量投影。
- search 简单标识符捷径改用无 body 的轻量查询，行号按符号范围回读源文件定位（单文件上限 10MB）。
- community 在索引超过节点上限时先拒绝再加载快照；快照被截断时在报告中输出警告。
- 跨项目 DB 缓存的 pending-close 条目可复活复用；新项目打开失败时回滚驱逐操作。
- Display / daemon wire version **0.9.3**。

## [0.9.2] - 2026-08-18

### Fixed
- 重索引定义文件时先 `ParkInboundRefsForFile`，CASCADE 删节点后跨文件 `calls`/`references` 边能经 `ResolveForFiles` 接回。
- 目录 `Remove`/`Rename` 走 `DeleteTree`；`IndexAll` 会 prune 盘上已消失的路径，不再留幽灵节点。
- `ResolveAll` / `SynthesizeAll` 失败并入 `Index*` 返回值，不再标 schema 成功。
- 合成某一 pass 失败时不调用 `ReplaceSynthesizedEdges`，避免用残缺集合整表覆盖。
- JSX 同名组件按文件/目录远近选择，不再全局 first-wins。
- `isGoToolchainPath` 只跳过 GOPATH 根（`/root/go`、`$HOME/go`、`/home/*/go`、`/Users/*/go`），`sdk/go`、`internal/go`、`clients/go` 会进索引。
- FTS `MATCH` 只查 `name`/`body`，搜 `go` 不会因 `language` 列刷屏。
- `store_fact` 的 insert 与 supersede 同一事务；失败整单回滚。
- home-mode 空 `path` 不再对整棵 `$HOME` 跑 rg；显式 `path=` 仍允许。
- `no_ignore=true` 不走 FTS 捷径，会落到带 `--no-ignore` 的 rg。
- `search_facts` 与 `store_fact` 共用 `targetFile` 规范化，绝对路径能搜到刚存的 fact。
- `affected` 输入全部被丢弃时写明原因，不再只报 `No affected test files found.`
- explore 在 workdir 是 symlink 时仍按 `path=` 过滤；`PathUnderRoot` 对相对入库键也能对上。
- Pi `skipCode=false` / `includeCode=true` 不再剥代码围栏和 `file:line:content`。

### Changed
- Display / daemon wire version **0.9.2**。

## [0.9.1] - 2026-08-18

### Fixed
- **Pi extension** (`integrations/pi/codegraph-go.ts`): child process lifecycle diagnostics no longer use `console.error`/`console.warn` on the host process (those writes corrupt Pi's TUI input row). Logs go to `~/.pi/agent/logs/codegraph-go.log` unless `CODEGRAPH_GO_DEBUG` is set; intentional stop and clean exit 0 stay quiet; unexpected non-zero exits still dump filtered stderr.
- `path=` 过滤按 `StoragePath` 比较，生产相对 key 能对上绝对/相对子目录；home-mode `path=项目名`（含末尾 `/`）先选项目再剥前缀，不再 404。
- `resolveDefs` 的 FTS 回退只留名字精确匹配，正文命中不再被当成定义走调用图。
- 简单标识符 `search` 输出正文命中行，不再总是符号起点。
- `callers` 打印边上的调用点行（`e.line`）；`callees` 仍用定义行。
- 默认 `skipCode=true` 时不再声称「源码已包含 / 当 Read 过」；说明书同步。
- `detectProject` 只认精确目录名或 `项目/子路径`，句子里出现项目名不再切库。
- `store_fact` 去重改为 `(content_hash, target_file, symbol)`；同一句话可挂两个符号。旧库在 `ensureSchema` 迁唯一键，不抬 schema revision、不强制全量重建。

### Changed
- Display / daemon wire version **0.9.1**。

## [0.9.0] - 2026-08-08

### Added
- Go interface 方法现在提取为 `kind=signature` 节点（此前作为成员方法丢失）。
- config 的 `workdirs` 与 `-workdir` 支持 `~` 与 `$VAR`/`${VAR}` 展开
  （`expandPath`）：`~/proj` 与 `$PROJECT_ROOT` 按用户预期解析，展开为空
  的条目被跳过；Pi 适配器授权根同步同一展开语义，避免 `~/proj` 变成字面
  的 `.../~/proj` 导致会话 workdir 对不上。
- 新增可选环境变量 `CODEGRAPH_MCP_TOKEN`：daemon unix socket 的可选 token
  鉴权，默认关闭（不设即行为不变）。
- safelog 新增日志脱敏：敏感键整值打码 + 常见密钥形状（PEM 私钥、
  Basic/Bearer、JWT、sk-/ghp_/AKIA/AIza/xox/Stripe 等）正则打码，覆盖
  slog 与 legacy `log.Printf` 两条路径。能力边界：best-effort，不替代源头
  不记录密钥——匹配不到已知形状的密钥仍可能出现在日志里，密钥不应写入日志。

### Changed
- **BREAKING:** `search` 默认字面匹配（`--fixed-strings`），`regex=true`
  才启用正则语义；`search` 默认尊重 `.gitignore`，`no_ignore=true` 才加
  `--no-ignore` 扫忽略文件。服务端 schema 与 Pi 适配器同步新增 `regex` /
  `no_ignore` 布尔参数（默认 false），MCP 客户端不再能靠旧契约静默得到
  正则/忽略扫描行为。
- `$CODEGRAPH_CONFIG` 指向不存在的文件时被跳过，继续默认查找
  （./codegraph-config.yaml → ~/.config/codegraph/config.yaml），调用方
  不再追死路径；Pi 适配器同步该查找语义。
- Display / daemon wire version **0.9.0**。
- Index schema revision **18 → 19**：0.9.0 提取语义变更强制全量重建（见
  Migration）。

### Fixed
- Go 多行 import 在 tree-sitter 主路径丢失 imports 边。
- JS `require()` 调用不提取。
- C-like / Rust / Ruby / PHP 调用边行号错位。
- 同名符号跨文件错连。
- watcher 失败不回队导致静默漏索引。
- symlink 替换后幻灵索引（旧目标条目残留）。

### Security
- daemon 与 tool 分发层加 recover：panic 转为单次调用错误，不击穿共享
  daemon 进程。
- 所有 tool 参数加服务端硬上限（结果数、深度、offset/limit 等 clamp）。
- 全部 action 加超时（search/files/node/graph/facts 30s，communities 60s）。
- `store_fact` 内容限额：写库上限 + 回读截断，防事实表膨胀成磁盘 DoS。
- rg 输出改流式读取，不再整体吞入内存。

### Migration
- `search` 迁移：需要正则匹配时显式传 `regex=true`；需要扫忽略文件时传
  `no_ignore=true`。
- **自动全量重建（schema rev 18 → 19，必须）**：0.9.0 的提取语义变更（Go
  接口方法 `kind=signature` 节点、Go 多行 import 边、JS `require()` 调用、
  调用边行号修正、同名符号跨文件消歧）使旧索引的节点/边过时；`content_hash`
  增量门会跳过未变文件，旧语义不会随增量索引逐步自愈。本版本把 index
  schema revision 从 18 抬到 19，升级到 0.9.0 后 daemon 首次启动检测到
  revision 不匹配，自动清空符号索引并全量重建——无需任何手动操作。重建在
  daemon 内持锁执行（期间查询可能返回部分结果），耗时与库规模相关（相当于
  一次完整初始索引）；重建完成后 `status` 的 schema 显示为 19。

## [0.8.10] - 2026-08-07

### Fixed
- KillStaleDaemon SIGKILL 前 PID 复用复检（daemon）：SIGTERM 宽限期后升级
  SIGKILL 前重新核对进程身份（`verifyDaemonIdentity`，与 terminateViaKill
  同一先复检再发信号纪律）——宽限窗口内 daemon 可能已退出、pid 被无关进程
  复用，直接发信号会误杀无辜进程；身份不符时若 pid 已死则清 pidfile 并成功
  返回，若 pid 活着但已非本 daemon 则拒绝并保留锁，绝不 SIGKILL 身份无法
  确认的进程。
- detectProject 并发快照（server）：匹配循环改为遍历锁内拷贝的
  DetectDirs 快照——resetDetect 可并发清空/替换该切片，原实现遍历活切片
  存在数据竞争；快照保持缓存语义不变。
- ReplaceFileIndex placeholder 越界改报错回滚（db）：realID 对超出
  batch/module 两段范围的负 placeholder 返回带诊断信息的错误，事务整体
  回滚，不再越界 panic。
- SearchFacts LIKE 通配符按字面匹配（db）：查询串中的 `%` 与 `_` 经
  escapeLikePattern 转义并配 `ESCAPE '\'`，用户搜索不再被通配符展开。
- ToolAffected symlink/路径逃逸封堵（tools）：词法围栏（`../` 与 workdir
  外绝对路径跳过）之上新增 real-path 边界检查，workdir 内指向外部的
  symlink 视为逃逸拒绝；已删除文件按最深现存祖先解析，仍正常接受。
- store_fact 路径逃逸封堵（server）：targetFile 双层检查（词法围栏 +
  EvalSymlinks 真实路径，同 resolvePathIn），拒绝 root 外绝对路径、`../`
  逃逸与 symlink 逃逸；未创建的目标文件按最深现存祖先判定，合法未来文件
  仍允许。
- RebuildAll schema revision 失败不再假成功（extraction）：
  SetSchemaRevision 失败时把错误传播给调用方，不再仅打日志后返回 nil——
  reindex 成功但索引未被认证为最新时不得伪装成功。
- WipeIndex 文档修正（db）：注释更正为 schema/meta/schema-revision 全部
  保留，revision 只由重建成功后的 SetSchemaRevision 更新，清空未重建的
  索引继续被 NeedsRebuild 判真。

### Changed
- Display / daemon wire version **0.8.10**。

## [0.8.9] - 2026-08-07

### Fixed
- pidfd 泄漏（0.8.9）：`IsProcessAlive` 改用 `kill(2)` signal 0 探测
  （`syscall.Kill(pid, 0)`），不再经 `os.FindProcess`——Go 1.24+ 工具链在
  Linux 上每次 `FindProcess` 打开一个 pidfd 且不释放，PPID watchdog（默认
  每 5s 一次探测）令每个进程无界泄漏（实测 96→99/15s）。kill(2) 零 fd
  分配；语义不变：nil → 活，ESRCH → 死，EPERM → 活（非我们的进程），其余
  错误保守按活。`KillStaleDaemon` 与 invisible-holder 回退路径
  （`terminateViaFindProcess` → `terminateViaKill`）同步改 `syscall.Kill`，
  保持先复检再发信号纪律；`terminateViaPidfd` 主路径保留。新增回归测试
  `TestIsProcessAliveNoFdLeak`（对活 pid 连续 200 次探测，断言
  `/proc/self/fd` 不随调用增长；修复前 7→207 红，修复后绿）。

### Changed
- Display / daemon wire version **0.8.9**。

## [0.8.8] - 2026-08-07

### Fixed
- 信号窗口用 pidfd 收死（m2）：invisible-holder 终止改用
  `terminateViaPidfd`——`pidfd_open` 把进程固定为文件描述符后再复检
  `liveInvisibleHolderMatch`，通过后 `pidfd_send_signal` 发 SIGTERM/SIGKILL，
  信号目标被 pidfd 钉住，pid 死亡+复用不再能劫持信号（recheck→signal 的
  TOCTOU 窗口关闭）；同一 pidfd 复用于 SIGKILL 升级，进程已死则 ESRCH 安全
  no-op。pidfd 不可用（非 Linux、ENOSYS、EPERM 等）时优雅回退到原有
  os.FindProcess+Signal+复检路径，回退逻辑封装为可注入变量（signalFn 风格，
  供测试）。新增 `golang.org/x/sys/unix` 直接依赖。
- invisible 分支新生 daemon 宽限（m3）：`killInvisibleHolderProcs` 杀前读
  `PidPath(root)`，pidfile 存在且 `DecodeLock.StartedAt` 距今 < 3s（且记录
  进程存活）时跳过杀——给刚 spawn、正在 onReady 的新 daemon 时间完成 bind
  （届时可 dial）或自行因 flock 失败退出，避免双客户端竞态误杀；楔子场景
  （pidfile 缺失）不受影响。
- flock 探测去 O_CREATE（m4）：`invisibleHolderHoldsLock` 改 O_RDWR（不
  create），无锁文件/目录不存在时一律视为 free，只读探测不再留下空锁文件。
- SIGKILL 仍存活 → 哨兵错误 + 主程序特判（m5）：
  `terminateInvisibleHolder`/`killInvisibleHolders` 对「SIGKILL 后仍存活」
  包装新哨兵 `ErrInvisibleHolderSurvived`（flock 仍被一个杀不死的进程持有，
  继续 spawn/direct 均不安全），`EnsureAndDial` 原样传播；main 在
  `ErrStaleDaemonRefused` 路径旁新增并列分支：stderr 打印含 root 与
  `pgrep -af codegraph-go` 手工清理提示的可操作信息并 exit 1，不落 direct。
- deploy.sh 的 /tmp sock 清理收窄到本项目（m6）：不再全局
  `rm -f /tmp/codegraph-go-*.sock`，只删本项目 workdir 的 tmp socket
  （`TMP_SOCK` 与 Go 侧 projectHash sha256(Clean(root))[:16] 对齐）。
- deploy.sh 自动 git commit 改 opt-in（nit）：仅当 `DEPLOY_COMMIT=1` 才自动
  提交，默认打印「改动未提交，如需自动提交设 DEPLOY_COMMIT=1」。
- 注释修正（m1）：cmd/codegraph-go/main.go 校验调用点注释与实际一致——
  WorkdirAllowlist 语义是无 config 时回落 $HOME 兜底、始终校验（fail
  closed），不存在「无 config 跳过校验」模式。

### Changed
- Display / daemon wire version **0.8.8**。

## [0.8.7] - 2026-08-07

### Fixed
- Pi 适配器手动 `codegraph-start` 的状态机：`getClient` 优先返回已启动的
  client 再判会话决策，手动拉起成功后把本次决策写回 `decision`，后续工具
  调用直接复用已启动客户端，不再出现「已拉起仍报尚未初始化/未启用」的
  假失败；`codegraph-info` 在会话已有决议时以其为准，展示与实际一致。
- Pi 适配器 config 解析支持 flow-style workdirs：`workdirs: [/root, /opt]`
  同行内联列表与 block 列表同样解析（可混排，支持引号与行尾注释），不再
  回落 $HOME 与 Go 侧 yaml.Unmarshal 分叉。
- Pi 适配器启动阶段不再二次决议：新增 `startClientAt` 直接使用已决议
  workdir（决议只发生一次：session_start / 手动 start），启动时不再
  consult `CODEGRAPH_GO_WORKDIR`，避免已过 allowlist 的决议 workdir 被
  env 改道且改道结果不再过授权校验。
- `deploy.sh` 杀进程前做完整谓词复检：新增 `daemon_matches`（argv0 基名
  ∈ {codegraph, codegraph-go} + `-workdir` 精确匹配 + environ 含
  `CODEGRAPH_DAEMON_INTERNAL=1`），对每个待杀 pid（含 pidfile 读出的）
  在 SIGTERM 前与 SIGKILL 升级前各复检一次，杜绝 PID 复用误杀窗口；
  `scan_daemon_pids` 复用同一函数，消除重复逻辑。

### Changed
- Display / daemon wire version **0.8.7**。

## [0.8.6] - 2026-08-07

### Fixed
- Pi 适配器改为按需拉起：`session_start` 不再全局 spawn codegraph 客户端，只在
  会话开头做工作目录决策与工具注册；客户端在首次工具调用时才真正启动，进程
  异常退出不再定时重启（下一次调用自动拉起）。
- Pi 适配器在会话开头按 config 授权根判定可用性：`makeWorkdirDecision` 在
  `resolveWorkdir` 之后对最终 workdir 做路径段级授权范围校验（与二进制
  `ValidateWorkdirs` 同语义，config 文件优先级一致，无 config 回落 `$HOME`），
  越界会话不注册工具、注入「本会话 CodeGraph 未启用」并拒绝 execute。

### Changed
- Display / daemon wire version **0.8.6**。

## [0.8.5] - 2026-08-07

### Fixed
- Config `workdirs` are now enforced as the authority allowlist: a workdir
  outside the declared roots (or not a descendant of one) is refused before
  any mode is entered — client, daemon and direct alike — with canonical
  (abs + symlink-resolved) comparison so symlink escapes are rejected. With
  no usable config the allowlist defaults to `$HOME`, and when `$HOME`
  itself cannot be resolved validation fails closed (every workdir is
  refused). `init <root>` enforces the same guard so it is not a side door.

### Changed
- Display / daemon wire version **0.8.5**.

## [0.8.4] - 2026-08-06

### Fixed
- Invisible flock holder self-heal: when every candidate socket is
  unreachable yet the DB lock is still held by a daemon whose pidfile/socket
  were removed (deploy race), `EnsureAndDial` locates the holder via `/proc`
  (codegraph argv + `-workdir` match + `CODEGRAPH_DAEMON_INTERNAL=1`) and
  replaces it instead of looping forever. The holder's identity is
  re-verified against live `/proc` immediately before every signal (SIGTERM
  and the SIGKILL escalation) to guard against PID reuse between scan and
  signal; a `/proc` scan failure is treated as "no holder found" instead of
  a fatal error; and the held-lock probe is a lightweight `flock(2)` check
  rather than a full database open.
- `deploy.sh` no longer removes `daemon.pid`/`daemon.sock` while a daemon
  might still hold the DB lock: it kills every daemon-mode process for the
  workdir, waits for the flock to be released, and only then removes
  artifacts; pre-warm readiness now requires pidfile + socket + held lock +
  live process.
- The "database in use" error no longer reports "held by pid <self>" when
  the pidfile names the reporting process itself (the daemon's own
  freshly-written pidfile during an invisible-holder wedge).

### Changed
- Display / daemon wire version **0.8.4**.

## [0.8.3] - 2026-08-06

### Fixed
- rg fallback error handling: exit-1 (no matches) is now distinct from a
  missing ripgrep binary or a timed-out search, so failures no longer surface
  as "no results".
- Symlink escape guard: the index skips symlinked files, and `callees` reads
  only paths resolved inside the project root (path jail).
- `ListUnresolvedRefsByFiles` queries are chunked by 400 files to stay within
  the SQLite variable limit.
- Index worker panics are counted as extraction errors and no longer mark the
  schema revision as rebuilt on interrupted or failed passes.
- A failed extraction keeps the previously indexed symbols and skips writing
  the new content hash, so the next pass automatically retries.
- Partial failures are counted and surfaced as a startup warning instead of a
  silent pass.
- `DefReCache` is a bounded cache (no unbounded memory growth).
- `affected` and root-directory queries honor their LIMIT at the boundary.
- Daemon startup failure releases the pidfile lock, so no zombie lock holder
  blocks the next start.
- Proxy hello failure exits non-zero instead of hanging in direct mode.
- Pi adapter keeps stderr buffered for diagnostics.
- README wording fix for `.gitignore`.

### Changed
- Display / daemon wire version **0.8.3**.

## [0.8.2] - 2026-08-06

### Fixed
- Single-writer database lock: the daemon takes an exclusive process lock on the
  index before opening it, cleans up a stale daemon (PID identity-validated)
  before re-acquiring, and reports who holds the lock when it cannot — two
  writers can no longer fight over the same index.
- Rebuild safety: `NeedsRebuild` no longer triggers a full index wipe on
  arbitrary read errors; a failed extraction keeps the file's previously indexed
  symbols, single-file reindexing is atomic, and index errors are surfaced
  instead of silently swallowed.
- Call-graph edges: multiple call sites between the same symbol pair now produce
  distinct edges (unique key includes line/column) via a crash-recovery
  migration, and synthesized edges are replaced atomically in a single
  transaction. Schema revision 17 → 18.
- Incremental sync hardening: re-extract is gated on a content hash with
  millisecond-precision mtime comparison; git-assist clears the index for
  deleted/renamed files; watcher overflow triggers a full rescan and
  re-registers directory watches; unresolved-reference scans are pushed down
  into SQL and tolerate empty tails.
- Daemon lifecycle: graceful exit when the parent proxy exits (PPID watch),
  PID-reuse guard so only the true stale daemon is killed, idempotent `Close`,
  and the safelog no longer panics when written after close.
- Safety nets: the path jail resolves symlinks, `projectPath` workdirs are
  allowlisted, `affected` depth is clamped, and `callees` falls back to the
  definition location when there are no callers.

### Changed
- Display / daemon wire version **0.8.2**; index schema revision **18**.
- Go directive bumped to 1.25.12 with toolchain go1.26.5 (stdlib CVE fixes);
  removed a 43 MB binary that was accidentally committed.

## [0.8.1] - 2026-08-06

### Fixed
- Pi adapter action dispatch: `withToolDefaults` now switches on `args.action`
  instead of the legacy tool name (the single `codegraph` tool never matched the
  old cases), so `max`/`max_results` clamping and defaults, `explore.skipCode`,
  `node.includeCode` and `affected.depth` defaults are effective again.
- Pi adapter captures server `instructions` from the MCP initialize handshake
  and injects them into the agent system prompt (`## CodeGraph server
  instructions`), ahead of the adapter's own `## CodeGraph Tools` section.

### Changed
- Display / daemon wire version **0.8.1**.
- gofmt comment alignment in `internal/db/query.go` and `internal/tools/community.go`
  (formatting only, no logic change).

## [0.8.0] - 2026-08-05

### Changed
- **Breaking:** MCP surface is a **single tool** `codegraph` with required `action`
  (`explore`|`search`|`files`|`node`|`callers`|`callees`|`impact`|`status`|`affected`|
  `communities`|`store_fact`|`search_facts`). Former top-level tools (`explore`,
  `search`, …) are no longer registered — same handlers, one schema (less model
  noise / tokens). Any MCP host (Grok, Pi, others) uses this entry directly.
- Server instructions and Pi adapter updated: Pi calls MCP tool `codegraph` with
  the full payload (including `action`); no per-action MCP name fan-out.
- Display / daemon wire version **0.8.0**.

### Migration
- Replace `tools/call` name=`explore` with name=`codegraph` + `action`=`explore`
  (same for other former tool names).
- Grok: `cg__codegraph` / `cg-eqi12__codegraph` + `action` (server keys `cg` / `cg-eqi12`).
- Pi: keep tool name `codegraph`; install adapter from `integrations/pi/` and
  binary ≥ 0.8.0 together.

## [0.7.1] - 2026-07-26

### Added
- `communities` / `store_fact` / `search_facts` now accept a `path` arg as a home-mode project selector (directory name under a broad workdir), retargeted via `detectProject` — same convention as `search`/`callers`/`explore`. Fixes pi-extension clients that pass `path` instead of `projectPath`.

## [0.7.0] - 2026-07-26

### Added
- `communities` MCP tool: Louvain community detection on the project call graph to reveal module/component boundaries for global architecture questions ("how is this project organized?", "what are the main modules?"). Uses gonum's `graph/community.Modularize` with fixed seed for deterministic results, projects directed edges as undirected with provenance-based weights (exact=1.0, import=0.8, proximity=0.3, heuristic=0.1), excludes `contains` edges, and caps output by community size.
- Agent fact storage: `facts` table in SQLite + `store_fact` / `search_facts` MCP tools.
  - `store_fact`: write agent findings attached to code symbols; SHA-256 dedup; supersede chain support; returns same-target facts for contradiction detection.
  - `search_facts`: query by content substring, file, symbol, or status.
  - Facts survive `WipeIndex` (index rebuilds don't delete them).
  - Absolute paths are normalized to workdir-relative before storing.

## [0.6.2] - 2026-07-25

### Added
- A versioned Pi-specific adapter under `integrations/pi/` with installation and configuration documentation.
- GitHub Actions CI for tests, vetting, and binary builds.

### Changed
- Pi runtime context now injects only dynamic index information; fixed tool workflow guidance remains in tool metadata and skills.
- Display and daemon wire version updated to 0.6.2.

## [0.6.1] - 2026-07-25

### Changed
- Index schema **17**: store workdir-relative paths for files/nodes/edges (portable across machines/checkouts; forces full rebuild)
- Display version **0.6.1**

### Fixed
- `affected`: same-package test discovery works with real indexes (relative storage keys + absolute BFS normalization)
- `affected` / MCP: reject `stdin=true` at the MCP handler; CLI/offline may still use stdin in `ToolAffected`
- Multi-workdir: secondary roots keep an open DB + file watcher (no longer cold-index-and-close only)
- Multi-workdir: tool `projectPath` reuses the secondary watcher DB (no second Open / dual writer)
- `files`: surface real `rg` failures instead of always reporting "no files matched"
- Daemon spawn inherits parent `-config` and `-no-sync`
- `CountFilesUnder`: escape LIKE wildcards; treat empty/"." as whole-index count
- SQLite DSN: percent-escape path characters (`#`, `?`, spaces, `&`)
- Import edges prefer `module` nodes over same-named functions
- Config: log read/parse errors instead of silent ignore
- README version/schema drift vs code
- Restored `ResolveBestTarget` / `TruncateBody` unit tests (accidentally dropped during path refactor)

## [0.6.0] - 2026-07-23

### Added
- YAML config file support (`-config` flag > `$CODEGRAPH_CONFIG` env > `./codegraph-config.yaml` > `~/.config/codegraph/config.yaml`)
- Multi-workdir indexing: additional directories in `workdirs:` list are indexed into their own `.codegraph/codegraph.db`
- `detectProject` scans all workdirs for project markers, stores full paths
- `ToolExplore` / `ToolStatus` accept `workdirs` parameter; home-mode overview shows projects across all workdirs
- Cross-project DB cache: `resolveProject` uses `FindNearestCodeGraphRoot` to locate the correct DB, with LRU eviction and ref-counting
- `resolvePath` tries resolution across all workdirs

### Fixed
- All tool handlers now pass `args.Path` to `detectProject` — previously only name/query was checked, so `path=<project>` was silently ignored
- `cmd/codegraph-go/main.go`: added workdir deduplication after canonicalization
- `internal/config/config.go`: prepend dedup check now iterates all elements instead of only checking index 0

## [0.5.0] - 2026-07-22

### Added
- `tools/node.go`: `includeCode` defaults to `false` — symbol mode returns location + signature + call chain only, no source body
- `tools/graph.go` + `main.go`: `ExploreArgs` gains `SkipCode` field; when `skipCode=true`, code blocks are replaced with line-count summaries
- Extension `codegraph-go.ts`: auto-injects `includeCode=false` / `skipCode=true`; added `formatCleanText` to strip markdown formatting (aligns with Read tool style); system prompt and tool descriptions updated

### Fixed
- `extraction/orchestrator.go`: index worker goroutines now `defer recover()` to prevent panics from crashing the process
- `daemon/proxy.go`: `io.Copy` errors are now logged instead of silently swallowed
- `callees_fallback.go`: rg calls now have a 10-second timeout to prevent hangs

## [0.4.0] - 2026-06-XX

- **12 MCP tools:** search, search_fts, files, context, explore, callees, callers, trace, impact, node, status, affected.
- **SQLite index + FTS5:** symbols/edges/files in `.codegraph/codegraph.db`; `search_fts` for indexed symbol search.
- **FTS hardening:** escape free-text queries; backfill `nodes_fts` on upgrade from pre-FTS databases.
- **24 languages:** Go/TS/JS/Python tree-sitter; regex fallback for Rust, Java, C#, Ruby, PHP, C/C++, Swift, Kotlin, Scala, Dart, Lua/Luau, R, Objective-C, Svelte, Vue, Astro, Liquid, Pascal/Delphi.
- **17 framework route families:** Gin/chi/mux, Express, NestJS, Flask/FastAPI/Django, Laravel, Rails, Spring, ASP.NET, Axum/actix/Rocket, Vapor, Play.
- **Cross-language bridges:** CGo, Python C extensions, React Native/Expo, Swift ↔ ObjC.
- **Auto-sync:** fsnotify watcher with debounce; staleness warning when pending files exist.
- Background cold index so MCP initialize is never blocked; non-blocking stderr logger.
- Output caps and shared truncate helpers for agent token budgets.

## [0.3.1]

- Remove MCP tool `status` (full-workspace file/LOC walk). Use `explore` or `search` instead.
- Drop unused `sync.Mutex`, `encoding/json`, and dead helpers after status removal.

## [0.3.0]

- Remove redundant `codegraph_` prefix from tool names (breaking change for existing users).

## [0.2.4]

- Refactor toolSearch to use rg.Output() to avoid process kill/wait deadlocks on containers.

## [0.2.3]

- Resolve memory leak in toolStatus by skipping huge cache directories and streaming files.

## [0.2.2]

- Handle Python triple-quotes (''' """) in stripStringsAndComments.
- Add countLeadingSpaces helper.

## [0.2.1]

- Filter out comments/strings when checking open brace for function body detection.

## [0.2.0]

- Auto resolve directory pattern to recursive glob in toolFiles.

## [0.1.3]

- Fix duplicate function call suppression bug across different definitions.

## [0.1.2]

- Fix pseudo-definition search bug and mitigate search OOM risk.

## [0.1.1]

- Optimize status LOC scanning with cache, resolve trace truncation and readLines OOM.

## [0.1.0]

- Initial 9-tool ripgrep-based MCP surface matching colbymchenry/codegraph shape.

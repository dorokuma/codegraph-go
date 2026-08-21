# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

## [0.9.3] - 2026-08-21

### Fixed
- daemon unix socket 绑定时临时设置 umask 0077，堵住 socket 创建到 chmod 之间的组/其他权限窗口；最终权限仍为 0600。
- 同名候选超过 1 万条时不再被静默丢弃：`CollectCandidates` 改为分页遍历全部匹配。

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

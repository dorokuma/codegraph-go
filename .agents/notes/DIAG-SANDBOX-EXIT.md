---
status: active
superseded_by: ""
supersedes: ""
模块: "server" # daemon spawn / 启动诊断链
---

# DIAG-SANDBOX-EXIT：SpawnDetached 启动失败诊断链（daemon.log 打开失败 + 退出观测）

标记：[MARK-CG-DAEMON-R1-W67-20261104-01]（R1）；本轮 R2 追加标记 [MARK-CG-DAEMON-R2-W67-20261105-RESUME-01]。诊断基线 `ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5`。
本文件是 C 档诊断修复记录（诊断 ≠ 功能修复完成；会话私有索引 + 只读可执行入口的功能方案不在本分支实施）。

## R2（B 服务端同一门禁第二轮/最后一轮返修）记录

- 修前快照 S_B_R1：base=`ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5` + 五文件原样覆盖层，保全于本 worktree `.agents/evidence/cg-daemon-r2-20261105-resume-01/pre-r1/`（MANIFEST.md + SHA256SUMS，sha256 与任务书登记一致）。
- **[pattern:diagnostic-log-identity]（B1，本轮续修）**：R1 的 `os.Stat` 轮转与 `O_RDWR` 打开均跟随叶链接——软链 daemon.log 会把子进程输出与尾迹重定向到项目外，FIFO 也仅被 tail 跳过而非拒绝。本轮实现 `openDaemonLog`：`os.OpenRoot(.codegraph)` 作用域 `OpenFile`（unix 上等价叶级 O_NOFOLLOW 的拒绝语义，逃出类链接在打开时被拒）+ 打开后 `f.Stat` 普通文件复核 + 与叶 `Lstat` 的 `os.SameFile` inode 一致性复核；轮转判定改 `Lstat` 且仅普通文件参与。失败一律关闭自己的 fd 且不 spawn；叶目录保 EISDIR，叶软链归一化为 ELOOP（`errors.Is` 可识别）。边界如实：父目录换根/硬链接不承诺全免（任务书同口径）。
- **[pattern:incomplete-diagnostic-redaction]（B2，本轮续修）**：R1 遗留「scrub 后 `out[len-4096:]` 字节级二次裁剪」——裁剪点落在幸存记录内部时会截掉识别前缀（例：`Header Basic <值>` 变裸值片段），生产 safelog 无法识别即明文出站。本轮二次裁剪改为同一「完整行」规则：超限整记录丢弃、单记录超限安全省略、≤4096B、不劈 UTF-8。回归 `post-scrub_clamp` 子例走 `daemonLogTail` → 真实 `safelog.SetupLogger` 管道端到端断言。
- **[pattern:nondeterministic-watcher-test]（B3）**：换链测试由 `sleep 0.3` 改 ready 标记→父换链→release FIFO 读写握手；两个静默抑制测试就地改名 `TestWatchDaemonExitCleanExitStaysQuiet`/`TestWatchDaemonExitWarnSuppressedByLiveHolder`，由测试侧经 `spawnLikeProduction`（复制生产子进程形状：env 过滤+Setsid+stdio 同 fd）直接驱动真实 `watchDaemonExit`，返回后 close(done) 再断言，不新增生产全局回调、不扫 /proc/self/fd、不重构状态机。
- **[pattern:pidfile-observation-overclaim]（B4）**：告警文案定为 `spawned daemon exited; no live daemon recorded in pidfile`；注释/测试/CHANGELOG 同步，明确 `daemonAliveAt` 只读 pidfile、无 pidfile 的 flock 持锁（`invisibleHolderHoldsLock`，proxy.go）不可见，不改判据与 fallback。
- **[pattern:duplicate-log-open-regression]（B5）**：spawn_test.go 的 EISDIR 重复子项并入 `TestSpawnDetachedDaemonLogOpenFailureFailsClosed`（补 `errors.Is(EISDIR)`/root/logPath/`systemdRunArgsFn` 启动准备计数 0）；非 root EACCES 用例独立保留为 `TestSpawnDetachedUnwritableCodeGraphFails`（root 下照旧 SKIP，不冒充验收）；`TestSpawnDetachedNonRegularLogSkipsTail` 就地改为 `TestSpawnDetachedRejectsSymlinkAndNonRegularDaemonLog`（软链→普通 sentinel、>10MiB 目标软链、悬空软链、FIFO 四拒绝例 + 普通文件正向控制）。
- 门禁归属：本轮为 B 服务端 R2（最后一轮自动返修额度）；A 客户端 R2 额度 2/2 已耗尽，未触碰 `pi-extensions` worktree；w69/w6C 同文案新事件未与实例日志关联，不宣称同根或已修复。C 档真实入口（真实 PiG 扩展加载 + sbx reviewer-agent 形态 status）仍待独立 verifier 在受控副本执行，worker 不执行、不访问线上实例；服务端局部修复 ≠ CG 整体恢复。

## B1 例外返修记录（用户授权的一次性额外轮次，标记 [MARK-CG-B1-EXCEPTION-W67-20261105-01]）

- 原 R2 的 2/2 轮次记录不变；本轮为用户当次明确授权的局部返修，仅限
  `internal/daemon/spawn.go` + `spawn_test.go`，不改公共接口、不改其它 B 项。
- **[pattern:diagnostic-log-identity]（B1，例外续修）**：R2 的 `os.Root` 作用域
  `OpenFile` 并非真正的叶级 O_NOFOLLOW——叶链解析在目录内时 scoped open 会**跟链**，
  对同目录悬空相对软链会先由 `O_CREAT` 创建目标、再由事后 `Lstat`/`SameFile` 复核
  拒绝（拒已打开但目录被污染）。本轮 `openDaemonLog` 改用单次
  `unix.Open(logPath, O_RDWR|O_APPEND|O_CREAT|O_NOFOLLOW|O_CLOEXEC, 0600)`：内核在
  open() 时即对最终叶符号链接返回 ELOOP（O_CREAT 之前，不落地目标），O_NOFOLLOW
  仅约束叶、中间组件保留 cgdir.Ensure 支持的软链祖先形态；打开后 `f.Stat` 普通文件
  复核保留，errno 原样透传（ELOOP/EISDIR），失败关闭自身 fd。删除打开后
  Lstat/SameFile 串与 ELOOP 归一化 Lstat 兜底；边界如实：Ensure 与 open 之间的
  父目录换根仍不承诺绑定（任务边界同口径）。
- 新增 K1 子例 `within_directory_dangling_symlink`（软链 `daemon.log -> rel-target`
  相对同目录且目标预不存在）：断言 ELOOP、root/logPath、目标未创建（标记
  CG_B1_INTERNAL_TARGET_CREATED）、启动准备计数 0、有界返回；旧四例与普通文件
  控制不变。
- 修前 R2 原样覆盖层保全于 `.agents/evidence/cg-daemon-b1-exception-20261105-01/`
  （pre-r2/ + MANIFEST.md + SHA256SUMS + RESULTS.md）。红/绿判据由独立 verifier
  在新 clone 执行；**最终审查/校验未齐，本项不得记为已通过**。

## 一句话结论

> 历史记录（R1/R2）：本节为当时决策原文，非现行契约；告警措辞已由 B4 收窄为 `spawned daemon exited; no live daemon recorded in pidfile`，daemon.log 打开已由 B1 改为单次 `unix.Open` + `O_NOFOLLOW`，见上文 B1 例外返修记录。

`SpawnDetached` 曾有两处证据黑洞：daemon.log 打开失败仍盲拉子进程（输出全丢）、子进程 `Wait` 结果无人消费（早死零痕迹）。修复后打开失败直接报错（fail closed），统一 watcher 在「子进程带错误退出且无活 daemon 持锁」时留下有界脱敏的诊断 warn。

## 背景与发现（4 项 confirmed 分诊）

1. **[pattern:diagnostic-log-identity]** 初版 watcher 按 `logPath` 新 `os.Open` 取尾迹——不绑定真实子进程输出文件：轮转/rename 后可读到另一个 daemon 的日志，路径被软链替换可读无关文件，路径是 FIFO 时 `O_RDONLY` open 会永久阻塞（子进程是唯一写端且已退出，父进程写端早已关闭）。
2. **[pattern:incomplete-diagnostic-redaction]** 「先截 4KiB 再 scrub」会把凭据识别前缀截在窗口外（如 `token=` 落在边界外，tail 只剩无法识别的值片段），生产 safelog 也救不回；且脱敏替换串比原值长（`pwd=x` → `pwd=[REDACTED]`），scrub 后可超过 4KiB。
3. **过度断言**：watcher 阻塞整个 daemon 生命却写 "exited early / without taking the lock"——daemon 正常运行数月后死掉同样走此路径，措辞把长寿退出误称早死。
4. **测试缺陷**：两个同构 warn 用例重复；active-holder/scope 断言未建立真实条件；`Authorization: Bearer ...` 样例被第一条 key=value 正则整体消费，后续 bearer/JWT/GitHub 断言形同虚设。

## 决策（处置）

> 历史记录（R1/R2）：本节为当时决策原文，非现行契约；告警措辞已由 B4 收窄为 `spawned daemon exited; no live daemon recorded in pidfile`，daemon.log 打开已由 B1 改为 `unix.Open` O_RDWR|O_APPEND|O_CREAT|O_NOFOLLOW|O_CLOEXEC（无事后 Lstat/SameFile 复核），见上文 B1 例外返修记录。

- **同一文件身份取尾迹**：`daemon.log` 以 `O_RDWR|O_APPEND|O_CREATE` 打开，`*os.File` 交给 `watchDaemonExit`，tail 经 `ReadAt` 读同一 open file description——不按路径重开，轮转/软链替换不可能重定向；`O_RDWR` 还顺带消除 FIFO 阻塞语义，非普通文件（FIFO/设备）经 `f.Stat().IsRegular()` 跳过，安全省略摘要但保留 exit/root 事实。fd 所有权随 watcher/等待者转移，各错误路径显式关闭。不跟随链接扩权、不扩大源仓权限。
- **截断脱敏顺序**：读窗口 = 文件最后 4KiB；窗口边界上的残缺首行**整行丢弃**（凭据前缀恰在窗口外时值片段无法被任何模式重新识别，宁可弃证不泄露），只发完整行；`scrubDiagText` 作用于完整行文本后再钳制回 ≤4096 字节（标记被切无害，此时已无可还原的明文）。不全文件读取。
- **只陈述观测事实**：warn 消息改为 `spawned daemon exited; no live daemon holds the lock`，attrs 仅 error/exit_code/root/log/log_tail。不新增启动状态机、不改重试/回退判据。
- **测试合并与真实条件**：warn+tail+once 合为 `TestSpawnDetachedExitWarnsOnceWithLogTail`；live-holder 抑制用 `TryAcquireLock` 真实持锁（`TestSpawnDetachedExitWarnSuppressedByLiveHolder`）；scrub 每个声明模式给独立样例（`TestScrubDiagTextPinsDeclaredShapes`）；组合端到端断言 `TestDaemonLogTailBoundaries`（边界劈裂秘密零泄露、首行丢弃保留后续行、扩张后仍 ≤4KiB）；身份绑定 `TestSpawnDetachedLogTailBoundToOpenedFile`（确定性屏障：子进程 sleep，父进程 rename+软链替换路径后再让子进程死）与 `TestSpawnDetachedNonRegularLogSkipsTail`（预置 FIFO 不阻塞、无 tail 但有基本事实）。

## 基线回归注入（verifier 用）

`internal/daemon/cgdir_guard_test.go` 追加 `TestSpawnDetachedDaemonLogOpenFailureFailsClosed`，只用基线已有符号（`SpawnDetached`/`CodeGraphDir`/`resolveDaemonBinaryFn`），用 daemon.log=目录（root 下也成立的 EISDIR，不依赖 chmod）触发：

```sh
git clone --quiet --no-local <repo> /tmp/cg-base && cd /tmp/cg-base && \
git checkout --quiet ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5 && \
cp <worktree>/internal/daemon/cgdir_guard_test.go internal/daemon/ && \
go test -count=1 -run TestSpawnDetachedDaemonLogOpenFailureFailsClosed -v ./internal/daemon
# 基线：FAIL（err==nil，子进程被盲拉）；本分支：PASS
```

实测基线 FAIL：`an unopenable daemon.log must fail SpawnDetached closed`（实质失败非编译错）。用后即删 /tmp/cg-base。

## 被放弃的方案

- **/proc/self/fd 复制出只读 fd**：可行但 Linux 专属、语义绕；`O_RDWR` 单 fd 更直接且无权限扩大（0600 不变）。
- **lookback 回补识别前缀**：读窗口再向前多读一段拼回 `token=`——秘密仍可能截在回补窗口外，且「值长度」随 lookback 无界；选确定性更强的整行丢弃。
- **保留 "early" 措辞配启动状态机**：超出诊断范围，按分诊改为只陈述事实。

## 限制与遗留

- root 下 `unwritable .codegraph` 子测试 SKIP（chmod 不产生 EACCES）；真实 sbx EACCES 由 PiG/沙箱侧另行验证，本记录不冒充已通过。
- 跨**行**边界的秘密值（值被 `\n` 劈开、识别前缀在前一行已发出）仍是 safelog 既有能力边界，未在本轮扩大。
- 诊断 warn 属可观测性：客户端落 direct 模式的判据不变（dial 超时）；watcher 不引入新增重试。
- stdout 无诊断污染：warn 走 slog（生产 safelog scrub sink），非 fmt 打印。

### [pattern:isolation-cache-deviation] B1 独立验证：隔离偏离、补证与误述更正

- 范围：B1 用户当次额外一轮中的独立 verifier R/G/K；不是新代码返修。用户本次选择“仅补证、不重跑”：认可已观察到的红绿/K1功能结果，隔离偏离作为证据完备性事项提交 reviewer/oracle 按“有记录隔离偏离”裁决。此选择不等于已满足原私有缓存条件，也不豁免其他 C 档硬门槛，不授权未来任务自行降级隔离或在工具拒绝后继续试探。
- 原始取文出处：verifier 会话 `/tmp/herdr-role-sessions/default/role-verifier-f7989dc9/2026-11-05T14-52-25-384Z_01a1ac32-61a8-76a7-a900-64bd0ee305bd.jsonl`，行号为该JSONL的物理行号。此路径仅为临时原件定位；本小节保存必要原文摘录作为持久证据，不依赖/tmp文件继续存在。

**A. 已确认偏离（confirmed，具体影响待两审裁决）**

| 项 | 调用/结果行 | toolCallId | 原始结果文本 |
| --- | --- | --- | --- |
| TMPDIR | 29 / 30 | chatcmpl-tool-8dafe1b207b54650 | `env key "TMPDIR" is not in the allowlist (allowed: GO*, NODE_ENV, CI, CTXMODE_*, …)` |
| GOCACHE | 31 / 32 | chatcmpl-tool-8918e7f29ceac930 | `env key "GOCACHE" is not in the allowlist (allowed: GO*, NODE_ENV, CI, CTXMODE_*, …)` |
| GOMODCACHE（额外探针） | 34 / 39 | chatcmpl-tool-bc805c360cbc51f1 | `env key "GOMODCACHE" is not in the allowlist (allowed: GO*, NODE_ENV, CI, CTXMODE_*, …)` |

实际R/G/K调用位于65、108、110行，env均仅显式传入GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off；没有实现任务书要求的本格私有GOCACHE/GOTMPDIR/TMPDIR。verifier在上述拒绝后继续执行，不能以“没有写入源仓”替代完整的隔离要求。GOTMPDIR未设为私有目录属已知偏离；上述组合调用没有给出它单独被拒绝的原文，不能写成已逐键证实被拒。

边界更正：原任务允许读取已有Go依赖缓存，并没有要求GOMODCACHE必须私有。GOMODCACHE被拒这一原文保留，但不能仅因读取默认模块缓存，就称其违反了不存在的私有GOMODCACHE条款。主代理前次把GOMODCACHE列入强制私有项的表述撤回。

**B. GOFLAGS“注入失败”的说法被原文反驳（refuted-with-evidence）**

- 同一会话34行，toolCallId=`chatcmpl-tool-aa60460518c4b748`，调用env为 `{"GOFLAGS":"-mod=mod"}`，argv为 `["python3","-c","print('GOFLAGS='+str(__import__('os').environ.get('GOFLAGS')))"]`。
- 38行对应结果原文为：`GOFLAGS=-mod=mod`。这是成功回显，不是注入失败。主代理前次对此项的误述在此明确更正，不能将“GOFLAGS注入失败”作为已证实缺陷入账。
- verifier报告没有交代这次成功探针。R/G/K的后续实际调用没有显式传GOFLAGS；不能由一次探针成功推定测试时继承的GOFLAGS值，也不能反推它注入失败。这一报告遗漏及其影响交两审评估，不补跑、不猜测。

**C. 已核实的功能结果与其证明范围**

- R：调用65行、结果66行，toolCallId=`chatcmpl-tool-89a3d77cb08fcc4a`。实际输出包含：
  ```text
  CG_B1_INTERNAL_TARGET_CREATED dangling-link target …/.codegraph/rel-target was created by the refused open: <nil>
  WRAPPER_EXIT=0
  verifier-expect-fail: PASS inner_exit=1 match="CG_B1_INTERNAL_TARGET_CREATED" tests=2
  ```
  上述路径仅在本记录脱敏省略，标记、退出码和计数均来自实际输出。R构造为BASE+原R2五文件覆盖层，再仅注入候选spawn_test.go；不是裸BASE或旧pre-r1。R清单哈希：ffcefdc98cb7c78014c39f666ccab1d2659168f44056e6a31ab7ad5463b41e60。
- G：调用108行、结果109行，toolCallId=`chatcmpl-tool-9c385b8060a530ed`，原文 `EXIT=0`，目标 `within_directory_dangling_symlink` 实际PASS，父+子RUN=2、无SKIP。
- K：调用110行、结果111行，toolCallId=`chatcmpl-tool-9e94d4f9dde63f46`，原文 `EXIT=0`；父RUN=1、指定六子例RUN=6，六子例均PASS、无SKIP。六子例为symlink_to_regular_sentinel、symlink_to_oversized_target、dangling_symlink、fifo_leaf、within_directory_dangling_symlink、regular_file_control。
- G/K清单哈希相同：a020a5698d0d40e36c298269f5ffb79bc22bf1d8bad5615406d8f636d2240052。功能结果与隔离条件分开记录；不把缓存偏离本身推定为错误结果，也不把功能PASS推定为隔离要求已达标。regular_file_control的marker只证明子进程运行过，不证明已回收或无进程残留。
- 上述仅是三个已执行判据，不等于完整CI、独立三轮race或真实PiG+sbx入口已执行。其他C档未闭环项保持原状态。未提交、未合并、未安装或部署；本补证不改变候选代码与原轮次账。

## 验证命令（本 worktree）

```sh
gofmt -l internal/daemon/   # 空输出
go build ./...              # exit 0
go vet ./...                # exit 0
go test -count=1 -v ./...                      # 全绿
CGO_ENABLED=1 go test -race -count=1 -v ./internal/daemon  # 全绿（root-skip 不计失败）
```

R2 红绿判据（K1-K4）的命令、exit 码、断言 ID 与 clone 哈希记录在 worker R2 报告与 `.agents/evidence/cg-daemon-r2-20261105-resume-01/` 下，独立 verifier 复核前以报告为准。

## 来源

- 基线 Commit：ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5；分支 fix/cg-sandbox-diag（worktree）。
- R1 worker w67:p7 / 初审 pA；R1 worker pB / 复审 pE（同 pane 续审）；R2 本 worker。A 客户端额度耗尽，不再改动。

## B1 第二意见、分诊与未闭环清单（2026-11-06）

1. **来源与对照**：oracle w67:pT 完整报告（已交付关闭）+ 主代理源码核验；相对 pre-r2，spawn.go 只变 unix import / openDaemonLog 与注释，spawn_test.go 只增既有表内 29 行同目录悬空链案例（主代理已精确对照）。B1 叶 O_NOFOLLOW 机制及相应功能结果可采，未发现 B1 例外增量的新确定 must/should。reviewer 唯一文档 should（CHANGELOG 旧 os.Root 机制、notes 当前式旧措辞）经 worker 同步、主代理原件核验与 oracle 第二意见确认，分诊 confirmed→resolved；不视为 reviewer 整审已完成。
2. **独立 R/G/K 三层结果**：R inner_exit=1、CG_B1_INTERNAL_TARGET_CREATED、tests=2、WRAPPER_EXIT=0；G EXIT=0 父子 RUN2 且目标 PASS；K EXIT=0 父子 RUN7、六子例全 PASS（原输出经主代理与 oracle 核验，持久摘录见上文 isolation-cache-deviation 节）。功能结果可采 ≠ 私有缓存条件满足，不重跑；GOFLAGS 成功探针非失败，GOMODCACHE 非原强制私有项；不得以 Go 缓存机制泛化证明所有隔离偏离均无影响。
3. **O1 [pattern:pidfile-read-leaf-trust] 分诊**：a) confirmed 静态事实——daemonAliveAt 的 os.ReadFile(PidPath(root)) 无叶类型/NOFOLLOW 检查；lock.go TryAcquireLock/ClearStaleLock、proxy.go:207 halfDeadDaemonHoldsLock、recovery.go:136 newbornDaemonInStartupGrace 亦如此。daemonAliveAt 函数体及 lock/proxy/recovery 文件与 BASE 一致，B1 例外未新增该读面；FIFO 无写端可阻塞，软链可引入外部 LockInfo 影响判定；不宣称真实秘密已泄漏，也不称能绕过 KillStaleDaemon 身份校验杀错进程。b) B1 外观察，但完整诊断 delta 影响 unresolved——BASE 普通分支为 cmd.Wait→缓冲 channel，当前新增 watchDaemonExit 在 Wait 后调用 daemonAliveAt 并 defer 关闭 logFile；不能把“函数本来存在/非 B1 新增”扩大为“整个诊断 diff 没有新增挂起或资源滞留”。整体影响与等级待隔离反例实测，当前不据此开始代码返修，也不按 oracle 一句“非本轮引入”给整个 C 门禁放行；oracle 的“无双 Wait/无 fd 泄漏”仅适用于已读分支且 pidfile 读取能正常返回的前提，非 FIFO 反例的反证。
4. **O1 探针状态**：仅规格、未执行、未预注册为已运行证据——临时 root 创建真实 .codegraph 与 daemon.pid FIFO；systemdRunArgsFn 固定 /bin/false、resolveDaemonBinaryFn 指向假 daemon，CPUQuotaPercent=150；SpawnDetached 用 done+3s 观测同步不返回；无 quota 非零退出变体观察 watchDaemonExit 卡在 pidfile 读取（不以 marker 替代完成/清理证据）；软链→合成合法 EncodeLock 且 PID/ProcStart 匹配存活测试进程变体观察判定被操纵。未来须独立 clone、超时与 FIFO 释放/子进程清理、旧 BASE/候选对照；本步不新建测试、不跑命令。是否验证或独立立项由用户决定。
5. **门槛仍缺**：reviewer 四支柱分母与 BASE→候选完整映射；全适用 CI（.github/workflows/ci.yml 的 test/vet/build/race/gofmt/staticcheck 等）；独立 3 轮 race；真实 PiG+sbx 受体入口（冷/热/并发两个 reviewer、指定 worktree 结果、有界失败与清理）。本轮 oracle 不能替 reviewer，R/G/K 不能替全 CI，摘要或 gate_scan 分类器不能决定放行。oracle 未重算组合清单/逆变换哈希；主代理既有证据照实保留，不写成 oracle 独立重算已完成。
6. **结论一致性**：reviewer/oracle 无相反的 B1 机制结论；oracle 新增 O1 独立登记，文档修正前后结论差异来自快照改变，不是相互否定。原 reviewer 续审中断为 depth=6 参数超过 5（按已补取原报告），不是第二次敏感守卫命中，不改守卫。
7. **额度与状态**：原服务端自动 2/2 已耗尽 + B1 一次性例外账不变，客户端 2/2 耗尽未碰；当前整体 C 未通过，gate_scan 由主代理在记录核验后汇总、尚未执行，不伪造扫描结果；不提交/合并/安装/部署。
8. **模式复发检索**：主代理已在 notes 精确搜索 pidfile-read-leaf-trust 未见既有项，本次新登记。diagnostic-log-identity 在初版、R2 与例外轮都有记录，已由 K1 尤其 within_directory_dangling_symlink 形成可机械执行的回归；晋升方向是将这些叶安全回归作为持续校验保持，是否再为 pidfile 读面增独立持久测试由用户决定，本步不实施。

## O1 本轮约定 env 字段入口受阻事实与遗留裁决输入（2026-11-06）

标记：[MARK-CG-O1-RECORD-W67-PZ-20261106-01]

1. **来源**：原 verifier 完整报告已交付并关闭；主代理按 §9 由原会话补齐破损字段，不重跑。原会话 = `/tmp/herdr-role-sessions/default/role-verifier-bbb1016c/2026-11-06T01-48-12-940Z_01a1ae8a-c70c-75d9-aac8-fed0883391c0.jsonl`；此路径只作原件出处。
2. **五分类唯一**：未执行-拒答（本轮约定的 ctx_run env 字段隔离前提未满足）；无 clone、无判据运行、无测试数/inner 或 EXIT；既不证明 O1 存在也不证明不存在。
3. **拒绝原文**：line29 调用 ctx_run argv `[printenv,GOCACHE,GOTMPDIR,TMPDIR,GOTOOLCHAIN,GOPROXY,GOSUMDB]` 且 env 字段 6 个隔离键；line31 结果 `env key "TMPDIR" is not in the allowlist (allowed: GO*, NODE_ENV, CI, CTXMODE_*, …)`。line36 调用 argv `[printenv,GOCACHE,GOTMPDIR,GOTOOLCHAIN,GOPROXY,GOSUMDB]` 且 env 字段 5 键（去 TMPDIR）；line38 结果 `env key "GOCACHE" is not in the allowlist (allowed: GO*, NODE_ENV, CI, CTXMODE_*, …)`。工具级拒绝，无进程退出码。新精确标签记 [pattern:isolation-env-allowlist-blocked]（主代理已精确检索既有 notes 无同标签）。此两条拒绝仅证明本轮约定 env 字段入口的整包调用被拒：不证明 GOTMPDIR 单键被拒，也不证明所有合规隔离通道都不可行。
4. **佐证行**：同 line29 并行 argv `[env,GOCACHE=...,GOTMPDIR=...,TMPDIR=...,GOTOOLCHAIN=local,GOPROXY=off,GOSUMDB=off,go,version]`，line32 原结果 `go version go1.26.4 linux/amd64`；verifier 报告写 EXIT=0 但原始 toolResult 没有显式 EXIT 行，主代理按 §9 只采信 argv 命令真实执行和 stdout 原文，不把报告措辞当显式退出码；P2 仅执行 go version，未用于运行 A/B 判据；不能据此认定隔离前提成立或整个流程无偏差。明确登记 [pattern:verification-contract-ambiguity]：原主代理任务书同时禁止 argv/env 旁路、又举 argv env ... go version 为允许前提示例，存在编排契约冲突/歧义，责任由主代理承担，不能全部归 verifier；P1/P2 同批并行，P1 拒绝后 P3 又另行拆键探查，与「一次性调用/拒绝后停止」约束不符。P2/P3 事实均不生成 O1 执行证据。
5. **判据 A 原样**：`go test -count=1 -timeout 20s -v -run '^TestO1PidfileReadProbe$' ./internal/daemon`；未执行。判据 B 原样：`verifier-expect-fail --expect-re O1_PIDFILE_FIFO_WATCHER_BLOCKED --reject-re 'build failed|undefined:|no test files|no tests to run|panic: test timed out|signal: killed|O1_PROBE_CLEANUP_FAILED' --min-tests 3 -- go test -count=1 -timeout 20s -v -run '^TestO1PidfileReadProbe$' ./internal/daemon`；未执行。主代理补取破损行，以任务书原文为准。
6. **O1 状态更新**：仍是 [pattern:pidfile-read-leaf-trust]。未执行事实本身不是新增代码缺陷；O1 完整诊断 delta 的 watcher-FIFO 影响仍 unresolved，其既有 C 门禁阻断未解除。当前没有隔离反例实证，不能视为已证实运行缺陷或已 refuted；本次仅按主控授权进行记录澄清和 oracle 定向裁决，不另探测/换通道/重跑探针，也不改代码；不得为了过门禁用非隔离执行冒充。
7. **误差与一致性**：报告头「SOURCE 八项」与列表实际七项不一致，主代理据末尾逐项核对按七项；verifier 临时照会目录已删除、无 clone；原 worker 报告 tag 补齐后哈希链和五文件未变。
8. **整体 C 门禁缺项不变**：reviewer 四支柱完整审查、全适用 CI、独立 3 轮 race、真实 PiG+sbx 入口；本条属遗留输入，不提交不合并，不启动返修。

## O1 新契约 env 字段实测仍受阻（2026-11-06）

标记：[MARK-CG-O1-RECORD2-W67-P15-20261106-01]

1. **新契约唯一形态**：新 verifier 任务契约已从旧稿「argv/env 旁路禁令与示例冲突」改为唯一允许形态「独立 clone + argv 执行 + ctx_run env 字段送入」；无旁路通道尝试。前提调用固定为 argv `[printenv,GOCACHE,GOTMPDIR,TMPDIR,GOTOOLCHAIN,GOPROXY,GOSUMDB]`，ctx_run env 六键。
2. **前提调用被拒原文**：原会话唯一一次前提调用被工具层拒绝，原文：`env key "TMPDIR" is not in the allowlist (allowed: GO*, NODE_ENV, CI, CTXMODE_*, …)`。无进程启动、无 EXIT；A/B 均未创建 clone、未执行 go/wrapper。A 判据完整预注册命令和 B 完整 wrapper 命令已在上节保留；不得把本轮当测试 FAIL/PASS/refuted。
3. **同标签复发累计 2 次**：本轮新增记录 [pattern:isolation-env-allowlist-blocked] 同一标签复发（此前已有 1 次，现累计 2 次同模式），但主控要求先让 reviewer 补四支柱裁决，再由 gate_scan 汇总；是否把此工具边界立为自动化持久约束或改用别的合规隔离路径由用户决定，本 worker 不实施。
4. **报告/证据偏差登记（供主代理分诊，不夸大）**：该次 verifier 有两处偏差：a) A/B 报告分类写「未执行」，按 verifier 契约应归「未执行-拒答（前提不满足）」；b) 报告另用无参数 env 读取全部环境、pwd、ls /tmp，超出固定步骤约束，其中 env 输出不回传，仅记「存在额外完整环境读取」；rm -rf 调用没有显式 EXIT；以「ls /tmp 入库后 kb 搜索无匹配」作删除实证证据力弱，但主代理核源码/notes/HEAD 均未变。不记录环境具体内容。
5. **证据链现状**：O1 watcher-FIFO 运行影响仍 unresolved，且仍没有任何两格执行证据；新 should 修复的 P2 EXIT 纠正句已恢复（当前 notes SHA 即包含），oracle 之前新 should 在本步前已补，不等于本轮 O1 执行成功。文档 should fix 闭环仅针对文本完整性，不降低门禁。
6. **整体门槛仍缺**：reviewer 四支柱完整审查、全适用 CI、独立 3 轮 race、真实 PiG+sbx 入口，以及 O1 两格执行证据。不提交/合并/部署；不得借「记录失败」放行。

## O1 修复与日志恢复轮记录（用户授权的一次性额外返修轮，C 档）

标记：[MARK-CG-O1-FIX-W67-20261106-02]

- **[pattern:pidfile-read-leaf-trust]（O1，confirmed→本轮修复）**：独立 verifier 实测无写端 FIFO pidfile 使子进程已回收后的 `watchDaemonExit` 永久阻塞于 `os.ReadFile` 的 open(2)（候选 FAIL 命中 O1_PIDFILE_FIFO_WATCHER_BLOCKED，`defer logFile.Close()` 永不执行；BASE 无此路径）。本轮 `daemonAliveAt` 改为单次 `unix.Open(PidPath(root), O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC)`：O_NOFOLLOW 在 open() 时对叶软链返回 ELOOP（不跟链出项目）、O_NONBLOCK 使 FIFO/设备叶打开立即返回；随后同一已绑定 fd 上 `f.Stat` 复核普通文件再 `io.ReadAll`——消除 Lstat→ReadFile 的叶换绑窗口。打开失败/非普通叶/读错一律降级为「无活 daemon 记录」返回 false；`IsProcessAlive`/`ProcStart` 判定不变；`TryAcquireLock`/`ClearStaleLock`/`KillStaleDaemon`/proxy/recovery 的 pidfile 读面不在本轮范围，未动。同一函数同时覆盖 watcher 与 quota 窗内联探测两调用点。
- **新增回归（每分支一条）**：`TestWatchDaemonExitFifoPidfileDoesNotBlock`（真实 `spawnLikeProduction`+done 握手驱动 watcher：无写端 FIFO pidfile 下子进程被回收、watcher 有界返回、warn 发出且含 exit_code/log_tail，logFile 经 watcher defer 释放——done 本身就是其执行证明）；`TestWatchDaemonExitRejectsSymlinkPidfile`（叶软链指向含活测试进程合法 LockInfo 的外部文件，open 阶段 ELOOP 拒链、按无活记录发 warn——修前 os.ReadFile 跟链解码伪造记录会错误抑制 warn，红）。普通文件正向控制沿用 `TestWatchDaemonExitWarnSuppressedByLiveHolder` 既有断言面（真实 TryAcquireLock 持锁抑制 warn），未重复造。
- **post-scrub clamp 测试日志恢复（should fix，本轮落地）**：`TestDaemonLogTailBoundaries/post-scrub_clamp` 此前 `safelog.SetupLogger` 后只 `cleanup()`+恢复 `os.Stderr`，未恢复 `slog.Default()` 与标准 `log` 输出/flags/prefix（cleanup 仅排空 writer）；现 SetupLogger 前保存四项全局状态，defer+显式双重路径原样恢复，失败路径同样生效。生产 SetupLogger 契约未改。
- **R2-RESULTS 更正注记**：`.agents/evidence/cg-daemon-r2-20261105-resume-01/R2-RESULTS.md` 已追加更正——「全局 logger 恢复」表述与当时实现不符，本轮已修，历史结论不改写。

### 测试规模书面说明（对照口径：基线 ae3dd51，本轮前累计 delta）

实测口径：测试 +963/-8（spawn_test.go +893/-8 + cgdir_guard_test.go +70/-0）对实现 +264/-65（spawn.go）；新增 ≈3.65×（963/264）；总增删 971/329≈2.95；净增 955/199≈4.80。以上为本轮开始前对基线 ae3dd51 的累计口径；本轮 O1 返修再增测试 +131/-0（spawn_test.go，新增 2 条测试函数 + post-scrub 子例日志恢复）与实现 +32/-1（spawn.go 的 daemonAliveAt 改造 + io import），含本轮后累计测试 +1094/-8、实现 +296/-66。

逐条新增测试函数与独立证伪面（标签对应前文 B1-B5/O1 分诊）：

| 测试函数（文件） | 证伪面 | 标签 |
| --- | --- | --- |
| `TestSpawnDetachedDaemonLogOpenFailureFailsClosed`（cgdir_guard_test.go） | daemon.log 打不开仍盲 spawn（EISDIR root-proof 触发；errno/root/logPath/启动准备计数 0/marker 未出现） | B5 |
| `TestSpawnDetachedUnwritableCodeGraphFails` | 独立 non-root EACCES 支路（root 下 SKIP 是环境事实，非验收） | B5 |
| `TestSpawnDetachedExitWarnsOnceWithLogTail` | 子进程非零退出+无活记录时恰一条 warn，含 error/exit_code/root/log/log_tail 与盘证 | B4 |
| `TestWatchDaemonExitCleanExitStaysQuiet` | exit 0 锁竞争输家不误报（done 握手后断言静默） | B3/B4 |
| `TestWatchDaemonExitWarnSuppressedByLiveHolder` | 真实 TryAcquireLock 活持锁抑制 warn（普通文件 pidfile 正向控制） | B4/O1 控制 |
| `TestSpawnDetachedScopeWindowMisfireGuards`（2 子例） | scope 窗内 clean-exit 静默 / 窗后非零退出 warn 一次且不 respawn | B4 |
| `TestDaemonLogTailBoundaries`（5 子例） | 读窗劈裂秘密整行丢弃、只丢首行、scrub 扩张后仍 ≤4KiB、post-scrub 完整行钳制端到端经真实 safelog | B2 |
| `TestScrubDiagTextPinsDeclaredShapes` | 每个声明脱敏形状给独立样例（原 blob 被 key=value 正整体消费、断言虚设） | B2 |
| `TestSpawnDetachedLogTailBoundToOpenedFile` | 尾迹读同一 open fd 非路径重开（ready→换链→release FIFO 确定性握手） | B1/B3 |
| `TestSpawnDetachedRejectsSymlinkAndNonRegularDaemonLog`（6 子例） | 软链×3+FIFO 叶拒绝+同目录悬空链不落地目标+普通文件控制 | B1 |
| `TestWatchDaemonExitFifoPidfileDoesNotBlock`（本轮） | 无写端 FIFO pidfile 不阻塞 watcher、按无活记录发 warn | O1 |
| `TestWatchDaemonExitRejectsSymlinkPidfile`（本轮） | 叶软链 pidfile（含活进程合法 LockInfo）不跟链、不误抑 warn | O1 |

每条函数对应一个独立证伪面，同分支不重复开用例；-8 行为被 `captureSlog`/`lockedBuffer` 替换的两段非线程安全 `strings.Builder`+slog 交换重复块。辅助函数（`captureSlog`/`lockedBuffer`/`waitForLogContains`/`fakeDaemonScript`/`spawnLikeProduction`/`waitWatcher`）为驱动真实 `watchDaemonExit` 与异步 warn 断言的最小基建，非堆同构用例；>3× 的原因正是每条用例都带真实夹具（FIFO/软链/换链握手/真实锁/真实 safelog 管道）而非表格展开，规模如实登记供 reviewer 裁决。

### BASE 对照归因（本轮登记）

- **`TestStopAfterDirSwapLeavesSymlinkTargetClean`**：裸 BASE（ae3dd51）同构 clone 下复现同样 socket-path 断言失败——长临时路径触发 tmpdir socket fallback，而测试硬断言项目内路径。归 environmental 既有缺陷，标 `[pattern:env-test-socket-path]`，与本分支改动无关。
- **`TestRunProxyDeliversTailResponsesAfterStdinEOF`**：BASE 对照 PASS、候选 3/4 轮 `bind: invalid argument`。保持 **unresolved**：sun_path 归因已收回——BASE 对照的临时路径前缀不同、不构成同构复现，不能归 environmental 也不能归本分支缺陷，如实记录待后续裁决。
- **verifier 协议偏离（如实登记）**：存在额外未注册的重跑与多个判据共用同一 clone 的偏离，功能结果与隔离条件分开记账，不以功能 PASS 推定隔离达标。
- **`staticcheck@2026.1` 环境不可用遗留**：本机模块缓存仅到 honnef.co/go/tools 0.8.1，`GOPROXY=off` 无法执行、未获联网授权，标 `[pattern:verification-env-staticcheck-unavailable]`。

### 本轮索引行

- O1 修复：`internal/daemon/spawn.go` `daemonAliveAt` → `unix.Open(O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC)` + 同 fd `f.Stat`/`io.ReadAll`，全部失败路径降级 false。
- 日志恢复：`internal/daemon/spawn_test.go` `post-scrub_clamp` 子例恢复 `slog.Default()`/`log.Writer()`/`log.Flags()`/`log.Prefix()`。
- CHANGELOG：[Unreleased] Fixed 段新增 `daemonAliveAt` pidfile 叶安全条目。
- 更正：`.agents/evidence/cg-daemon-r2-20261105-resume-01/R2-RESULTS.md` 末尾更正注记。
- 状态：不提交、不合并、不部署；O1 红/绿判据由独立 verifier 复核，本项不自行记为门禁已通过。

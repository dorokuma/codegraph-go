# O1 pidfile-read-leaf-trust 隔离探针 — 载荷与执行规格（未执行，仅准备）

标记：`[MARK-CG-O1-PROBE-W67-PX-20261106-01]`
状态：**本工作步只交付可复制载荷与说明，未执行任何 go test/build/vet，未改被审实现或既有测试，未 stage/commit。**

## 1. 取证对象（主代理已选定的最小变体）

O1 分诊见 `.agents/notes/DIAG-SANDBOX-EXIT.md`（分诊表 O1 条目）：`daemonAliveAt` 的
`os.ReadFile(PidPath(root))` 无叶类型/NOFOLLOW 检查。本轮只取**无 quota 的普通
spawn 分支 watcher 变体**做旧 BASE/当前候选反例对照：

- **候选 spawn.go**（worktree 修改态）：`SpawnDetached` 普通路径以
  `go watchDaemonExit(cmd, root, logFile, logPath)` 收尾；
  `watchDaemonExit` 先 `defer logFile.Close()`，`cmd.Wait()` 返回非零时调用
  `daemonAliveAt(root)` → `os.ReadFile(daemon.pid)`。若 `daemon.pid` 是无写端
  FIFO，该读停在 `open(2)` 上，watcher goroutine 被无限期挂起，`logFile` 随之
  滞留。
- **BASE ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5**：普通路径只有
  `go func(){ waitCh <- cmd.Wait() }()` 回收器，**无 `watchDaemonExit` 符号**；
  `daemonAliveAt` 仅存在于 systemd-run quota 分支内联路径。同一载荷在 BASE
  必然不出现该阻塞链——靠的是真实代码路径差异，不是测试内的版本/哈希分支。

**范围外**：quota/FIFO 与软链伪 LockInfo 两个变体本次不跑，不得把本探针结果外推
为它们已验证。

## 2. 载荷

`o1_pidfile_read_probe_test.go.txt` —— 一份 `package daemon` 的 Go 测试文本
封存。verifier 将其**原样逐字节**复制为每个一次性 clone 内的
`internal/daemon/o1_pidfile_read_probe_test.go`（两快照同一份载荷，无分叉）。
该文件不是加入生产测试树的新测试。

探针结构（行号以 txt 文件计）：

- `TestO1PidfileReadProbe`（约 91 行）：两个串行子例 `regular_control`、`fifo`，
  不 `t.Parallel`，不触碰其它测试。
- `runO1PidfileReadProbe`（约 108 行）：
  - 私有 `t.TempDir` root + 真实 `.codegraph`；`regular_control` 建普通空
    `daemon.pid`，`fifo` 用 `syscall.Mkfifo` 建 FIFO；`daemon.log` 保持普通文件。
  - 覆写 `resolveDaemonBinaryFn` 指向 root 内固定假 shell 脚本、
    `systemdRunArgsFn` 返回 nil（强制普通路径），调用**真实**
    `SpawnDetached(root, nil)`；两处 hook 在 spawn 返回后立即恢复并 `defer` 兜底。
  - 假脚本只在自己的 `$2`（=-workdir root）写
    `.codegraph/o1-child.pid` 后 `exit 23`：不跑真实 codegraph，不建共享
    daemon/systemd scope，不带凭据。
- 断言链（关键行）：
  - **O1_CHILD_REAPED**（`o1WaitReaped`，约 229 行）：先观测真实子 PID 文件
    （`o1WaitChildPID` 有界 3s），再要求 3s 内 `IsProcessAlive(pid)==false`
    （kill(0) 对僵尸报活，故为 false 即已 Wait 回收）；不以 marker 推断回收。
  - **O1_BLOCKED_CHAIN**（`o1ObserveBlockedChain`/`o1BlockedChainSnapshot`，
    约 237–276 行）：3s 内轮询本测试进程 goroutine stack，实证判据 = 同一
    goroutine 同时含 `daemon.watchDaemonExit(` + `daemon.daemonAliveAt(` +
    `os.ReadFile(`；`O1_STACK_FRAME` 只输出匹配帧（≤12 行），不回传全量
    goroutine 或环境串。不接受 goroutine 总数、固定 sleep、marker、fd 数替代。
  - **O1_FIFO_RELEASED**（`o1ReleaseFIFOReader`，约 299–324 行）：FIFO 释放用
    `syscall.Open(pidPath, O_WRONLY|O_NONBLOCK)` —— 与被 O_RDONLY 阻塞的读者
    即时配对，无读者时 ENXIO，释放侧自身不阻塞；配对后 close 使读者 ReadFile
    得 EOF。有界 2s，返回 ok/no-reader/na/error:*/timeout:*。
  - **O1_CLEANUP_COMPLETE**（约 172–173 行）：`child_alive`（须 0）与
    `watcher_gone`（`o1WaitWatcherGone` 有界 2s 无 watchDaemonExit/daemonAliveAt
    帧）。清理失败以独立 `O1_PROBE_CLEANUP_FAILED` 报 FAIL，不混入预期缺陷。
  - 判负（约 185–202 行）：**先取证、先清理，后 Errorf**。`fifo` 命中阻塞链才以
    `O1_PIDFILE_FIFO_WATCHER_BLOCKED` 报 FAIL；`regular_control` 命中阻塞链以
    非标记文本报“正向控制被违反”FAIL（不得命中缺陷标记）；候选不阻塞则正常 PASS。

边界时间：handshake 3s、reap 3s、阻塞观察 3s、FIFO 释放 2s、watcher 消失 2s、
轮询 50ms；`-timeout 20s` 充裕。

## 3. 预期/判据（对照实验，不是先红后绿的修复验证）

无 O1 修复版本存在，本项为**旧 BASE / 当前候选反例对照**。未实测前不得称
“修复后绿”，也不得把预期当结果。

| 快照 | 期望 |
| --- | --- |
| BASE `ae3dd51` | `EXIT=0`；父+两子例 RUN≥3，两子例 PASS 无 SKIP；各含 `O1_CHILD_REAPED` 与 `O1_CLEANUP_COMPLETE child_alive=0 watcher_gone=true`；fifo 子例 `O1_BLOCKED_CHAIN=false`（BASE 无 watcher 符号），`O1_FIFO_RELEASED=no-reader` 或 `ok` |
| 候选（当前 worktree diff 态） | fifo 子例预期 FAIL 且含 `O1_PIDFILE_FIFO_WATCHER_BLOCKED`；`regular_control` PASS；两子例均 `O1_CHILD_REAPED`、`O1_CLEANUP_COMPLETE child_alive=0 watcher_gone=true`；`O1_FIFO_RELEASED=ok`；inner 退出码非零 |
| 任一侧 | 若阻塞未出现则如实 PASS（真实证伪允许）；`O1_PROBE_CLEANUP_FAILED` 为基础设施失败，与缺陷信号严格分离 |

## 4. 登记命令（只写不执行；最终预注册由主代理在 verifier 任务书完成）

BASE clone：

```sh
go test -count=1 -timeout 20s -v -run '^TestO1PidfileReadProbe$' ./internal/daemon
# 期望 EXIT=0
```

候选 clone（预期 FAIL 必须包 verifier-expect-fail，禁止裸跑）：

```sh
verifier-expect-fail --expect-re O1_PIDFILE_FIFO_WATCHER_BLOCKED \
  --reject-re 'build failed|undefined:|no test files|no tests to run|panic: test timed out|signal: killed|O1_PROBE_CLEANUP_FAILED' \
  --min-tests 3 -- \
  go test -count=1 -timeout 20s -v -run '^TestO1PidfileReadProbe$' ./internal/daemon
# 期望 wrapper EXIT=0 且 inner 非零
```

最低证据量（供 verifier 核对）：RUN≥3（父+两子例）；各子例
`O1_CHILD_REAPED`/`O1_BLOCKED_CHAIN`/`O1_FIFO_RELEASED`/`O1_CLEANUP_COMPLETE`；
fifo 阻塞时至少一条 `O1_STACK_FRAME` 含 watchDaemonExit/daemonAliveAt/
os.ReadFile 链；无 `O1_PROBE_CLEANUP_FAILED`。

## 5. 隔离前提与既有教训（必读）

- 每轮新 `git clone --no-hardlinks`，移除 `origin`；BASE 仅注入本 txt 原样探针，
  候选完整同步待审 diff 内容 + 同探针。clone 用完即删（/tmp tmpfs 容量有限）。
- 本格私有 `GOCACHE`/`GOTMPDIR`/`TMPDIR`；`GOTOOLCHAIN=local`、`GOPROXY=off`、
  `GOSUMDB=off`；可只读已有模块缓存。
- **此前 verifier 的 ctx_run env 对 GOCACHE/TMPDIR 等被拒过**：不得带偏离悄悄跑，
  不得换 Python/env 脚本/`go env -w`/全局配置绕过已拒的隔离设置。不假定 worker
  权限等于 verifier。
- 若隔离前提无法满足：verifier 交“未执行-拒答/隔离前提不满足”原始事实（用户
  已许可），回归遗留裁决；不因此降低门槛。
- 本步 worker 未探测允许列表、未实际设上述 env、未执行测试；仅对新探针文本
  做过 `gofmt` 格式化（`gofmt -l` 已为空）。

## 6. 版本与登记哈希

- 工作分支：`fix/cg-sandbox-diag`，worktree
  `/root/workspace/.wt/codegraph-go/fix-cg-sandbox-diag`。
- HEAD = BASE = `ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5`（候选为未提交 diff）。
- 五文件登记（SHA256，本步未变）：
  - `internal/daemon/spawn.go` `552137b2e7f36c799a0a6dc531379ea19f0c65487cdde573e9bda8b9ec62312f`
  - `internal/daemon/spawn_test.go` `8b30eb28ee84db86d34e020e9a1d30983f4132a5d7944bf1506a0db6d868bdbc`
  - `internal/daemon/cgdir_guard_test.go` `be329966e060628a7225129de58e154dfafe884195ae817bbd614c0d230c99a7`
  - `CHANGELOG.md` `8702fc72def6b49008e43e813363a27302e6044d4bf0b8008bda018855cdf637`
  - `.agents/notes/DIAG-SANDBOX-EXIT.md` `16f885621f55759e4e857f602e5d0c5c1fc9a6e90a6a1cc3ef9e610c75962355`
- 本目录两文件 SHA256：见交付报告（`sha256sum` 记录）。

## 7. 后续（本步不做）

主代理审原件 → 派 verifier 按上规格在两个一次性 clone 独立实测 →
reviewer/oracle 按实证裁决 → gate_scan 汇总。本任务不提交、不合并、不部署；
quota/FIFO 与软链变体是否立项另由主代理/用户决定。

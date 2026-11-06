---
status: active
superseded_by: ""
supersedes: ""
模块: "server" # daemon spawn / 启动诊断链
---

# DIAG-SANDBOX-EXIT：SpawnDetached 启动失败诊断链（daemon.log 打开失败 + 退出观测）

标记：[MARK-CG-DAEMON-R1-W67-20261104-01]。诊断基线 `ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5`。
本文件是 C 档诊断修复记录（诊断 ≠ 功能修复完成；会话私有索引 + 只读可执行入口的功能方案不在本分支实施）。

## 一句话结论

`SpawnDetached` 曾有两处证据黑洞：daemon.log 打开失败仍盲拉子进程（输出全丢）、子进程 `Wait` 结果无人消费（早死零痕迹）。修复后打开失败直接报错（fail closed），统一 watcher 在「子进程带错误退出且无活 daemon 持锁」时留下有界脱敏的诊断 warn。

## 背景与发现（4 项 confirmed 分诊）

1. **[pattern:diagnostic-log-identity]** 初版 watcher 按 `logPath` 新 `os.Open` 取尾迹——不绑定真实子进程输出文件：轮转/rename 后可读到另一个 daemon 的日志，路径被软链替换可读无关文件，路径是 FIFO 时 `O_RDONLY` open 会永久阻塞（子进程是唯一写端且已退出，父进程写端早已关闭）。
2. **[pattern:incomplete-diagnostic-redaction]** 「先截 4KiB 再 scrub」会把凭据识别前缀截在窗口外（如 `token=` 落在边界外，tail 只剩无法识别的值片段），生产 safelog 也救不回；且脱敏替换串比原值长（`pwd=x` → `pwd=[REDACTED]`），scrub 后可超过 4KiB。
3. **过度断言**：watcher 阻塞整个 daemon 生命却写 "exited early / without taking the lock"——daemon 正常运行数月后死掉同样走此路径，措辞把长寿退出误称早死。
4. **测试缺陷**：两个同构 warn 用例重复；active-holder/scope 断言未建立真实条件；`Authorization: Bearer ...` 样例被第一条 key=value 正则整体消费，后续 bearer/JWT/GitHub 断言形同虚设。

## 决策（处置）

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

## 验证命令（本 worktree）

```sh
gofmt -l internal/daemon/   # 空输出
go build ./...              # exit 0
go vet ./...                # exit 0
go test -count=1 -v ./...                      # 全绿
go test -race -count=1 -v ./internal/daemon    # 全绿（1 个 root-skip）
```

## 来源

- 基线 Commit：ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5；分支 fix/cg-sandbox-diag（worktree）。

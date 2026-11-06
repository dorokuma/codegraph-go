# B 服务端 R2 判据证据（worker，非独立 verifier）

标记：[MARK-CG-DAEMON-R2-W67-20261105-RESUME-01]
worktree=/root/workspace/.wt/codegraph-go/fix-cg-sandbox-diag 分支=fix/cg-sandbox-diag
base_commit=ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5
快照 S_B_R1 原样覆盖层=./pre-r1/（SHA256SUMS + MANIFEST.md）

## 快照指纹

S_B_R1（pre-r1/ 内五文件，sha256 与任务书登记一致）：
- b50fd832890f6b377d39f03a33a414a6125e8d4c014a6f48ecab852c86b0df48 internal/daemon/spawn.go
- 39246c16c847fa6d9f2c3bf5e4324f506b2f971c057defe28a78ca7a9e6ac8d7 internal/daemon/spawn_test.go
- 28b20e458cae1ced1bac9b800b958d43d4fd372576198fdc9346ca668f1e9dac internal/daemon/cgdir_guard_test.go
- cdb338cafb32b8011f745a91c937d987eb8c683e6bd2581fb623d0310943ecf0 CHANGELOG.md
- e26dd62ce2fa7be3a4d7ffbeb883bf25a51a61ff62f00f7a49a13c8165fd43d1 .agents/notes/DIAG-SANDBOX-EXIT.md

候选（R2 修改后）：
- 22a252a7f7b92f3a415df7cdd28c92717bb5da50cff1cd79655cf64b2e73ee42 internal/daemon/spawn.go
- 3a91c4d7464daf227c6c3c55be878bb9429f556b78626715acc6362633592d55 internal/daemon/spawn_test.go
- be329966e060628a7225129de58e154dfafe884195ae817bbd614c0d230c99a7 internal/daemon/cgdir_guard_test.go
- 12050d2d10ed153d26a4132b4f8867c560185608e13bc361e37a591935c6bef9 CHANGELOG.md
- 3c81fa2690d3c4c2f216224ccf4da78e838c06d59bc49127704e7d7a6c6df3d7 .agents/notes/DIAG-SANDBOX-EXIT.md

以下 K1-K4 红绿结果均为最终内容复验版（spawn_test.go=3a91c4d7…，与候选指纹一致）。

clone 构造：git clone worktree → git remote remove origin → git checkout ae3dd51 →
红：覆盖 S_B_R1 五文件 + 注入候选 spawn_test.go/cgdir_guard_test.go（K3 例外：裸 base 仅注入 cgdir_guard_test.go）；
绿：覆盖候选 spawn.go/spawn_test.go/cgdir_guard_test.go（首轮复验同时覆盖 CHANGELOG.md 与 notes；文档对测试无影响）。
每条判据独立新 clone，用后删除（已执行 rm -rf）。

## K1 叶拒绝

- 红 clone /tmp/cg-r2-k1-red（已删）：`verifier-expect-fail --expect-re CG_B1_LEAF_ACCEPTED --reject-re 'build failed|undefined:|no test files|no tests to run|panic: test timed out|signal: killed' --min-tests 1 -- go test -count=1 -timeout 20s -v -run '^TestSpawnDetachedRejectsSymlinkAndNonRegularDaemonLog$' ./internal/daemon`
  - 结果（最终内容复验）：WRAPPER_PASS（verifier-expect-fail: PASS inner_exit=1 match="CG_B1_LEAF_ACCEPTED" tests=6）
  - 实质失败：4/4 拒绝子例（sentinel 软链、>10MiB 目标软链、悬空软链、FIFO）均报 CG_B1_LEAF_ACCEPTED（修前接受并盲 spawn）；regular_file_control PASS。内部退出码 1，非编译/超时红。
  - cleanup：红轮被盲拉的子进程为假 daemon 脚本，立即退出并被 watchDaemonExit 回收，无残留；无满 FIFO 写者（FIFO 子例的子进程照样立即退出）。clone 整体删除。
- 绿 clone /tmp/cg-r2-k1-green（已删）：同命令去 wrapper，EXIT=0，5/5 PASS（顶层 1 + 4 拒绝子例 + 1 控制），0.017s。拒绝路径单次 <<2s、不进入启动准备（systemdRunArgsFn 计数 0 断言）、sentinel/悬空目标未被创建或改写、无 daemon.log.1 误轮转、无子进程残留。

## K2 二次裁剪

- 红 clone /tmp/cg-r2-k2-red（已删）：`verifier-expect-fail --expect-re CG_B2_TAIL_CONTEXT_LOST --reject-re 同上 --min-tests 1 -- go test -count=1 -timeout 20s -v -run '^TestDaemonLogTailBoundaries/post-scrub_clamp' ./internal/daemon`
  - 结果：WRAPPER_PASS（PASS inner_exit=1 match="CG_B2_TAIL_CONTEXT_LOST" tests=2）
  - 实质失败：`CG_B2_TAIL_CONTEXT_LOST unmaskable secret fragment reached the sink (len=2780)` —— post-scrub 字节级裁剪截掉 'Header Basic ' 前缀，裸值片段经真实 safelog.SetupLogger 明文出站。
- 绿 clone /tmp/cg-r2-k2-green（已删）：EXIT=0，post-scrub_clamp PASS 2.00s（emit ≤4096B 尾迹、Q 片段不出现、UTF-8 有效、完整行边界、管道并发 drain + 全局 logger 恢复）。

## K3 原始诊断回归

- 红 clone /tmp/cg-r2-k3-red（已删，裸 ae3dd51 仅注入候选 cgdir_guard_test.go）：`verifier-expect-fail --expect-re CG_B5_EISDIR_BLIND_SPAWN --reject-re 同上 --min-tests 1 -- go test -count=1 -timeout 20s -v -run '^TestSpawnDetachedDaemonLogOpenFailureFailsClosed$' ./internal/daemon`
  - 结果：WRAPPER_PASS（PASS inner_exit=1 match="CG_B5_EISDIR_BLIND_SPAWN" tests=1）
  - 实质失败：err==nil 盲 spawn（CG_B5_EISDIR_BLIND_SPAWN），非编译红。
- 绿 clone /tmp/cg-r2-k3-green（已删）：EXIT=0，1/1 PASS 0.30s（errors.Is(EISDIR)、root/logPath 在错误中、systemdRunArgsFn 启动准备计数 0、marker 不存在）。

## K4 既有回收/就绪/告警行为

- 绿 clone /tmp/cg-r2-k4-green（已删）：`go test -count=1 -timeout 60s -v -run '^Test(SpawnDetached(LogTailBoundToOpenedFile|CleanExitStaysQuiet|ExitWarnSuppressedByLiveHolder|ExitWarnsOnceWithLogTail)|WatchDaemonExit(CleanExitStaysQuiet|WarnSuppressedByLiveHolder)|DaemonAliveAtRejectsReusedPID)$' ./internal/daemon`
  - 结果：EXIT=0，5/5 RUN+PASS 0.242s：LogTailBoundToOpenedFile（ready→换链→release FIFO 握手，fd 身份绑定）、WatchDaemonExitCleanExitStaysQuiet、WatchDaemonExitWarnSuppressedByLiveHolder（真实 TryAcquireLock 持锁抑制）、ExitWarnsOnceWithLogTail、DaemonAliveAtRejectsReusedPID。
  - 完成证据：静默断言在 watcher done（chan 关闭）之后执行；换链子进程经 ready marker + release FIFO 明确握手；无 sleep 屏障、无 /proc/self/fd 扫描；每个 child 由真实 watchDaemonExit 的 Wait 回收，fd 随 watcher defer 关闭。

## 本 worktree 自检（非判据替代）

- gofmt -l . → 空输出，EXIT=0
- go build ./... → EXIT=0（上游 smacker/go-tree-sitter C 源码 warning，非本仓错误）
- go vet ./... → EXIT=0
- go test -count=1 -timeout 180s ./... → EXIT=0，12 包全 ok（含 1 项 root-skip：TestSpawnDetachedUnwritableCodeGraphFails、TestHelperHoldFlock helper-skip）
- CGO_ENABLED=1 go test -race -count=1 -v -timeout 180s ./internal/daemon → EXIT=0，0 DATA RACE，全部 PASS（含 root-skip 1 项）

## 未执行项（交独立 verifier/reviewer/oracle）

- 独立 race 三轮（各新 clone）、staticcheck@2026.1、ripgrep CI 步骤、C 档真实入口（真实 PiG 扩展 + sbx reviewer-agent 形态 status、正常/只读拒绝控制、权限不变性证明）。
- w69/w6C 同文案事件未与实例日志关联，不宣称同根/已修复。

## 更正注记（[MARK-CG-O1-FIX-W67-20261106-02]）

上文 K2 绿 clone 小节「管道并发 drain + 全局 logger 恢复」的表述与当时实现不符：post-scrub clamp 子例当时只调用 `safelog.SetupLogger` 返回的 `cleanup()` 并恢复了 `os.Stderr`，但 `cleanup` 仅排空/关闭非阻塞 writer——`slog.Default()`、标准 `log` 包的输出目标与 flags/prefix 仍留在指向已关闭 writer 的 handler 上，并未恢复。本轮（O1 返修轮）已在 `TestDaemonLogTailBoundaries/post-scrub_clamp` 中补回：SetupLogger 前保存 `slog.Default()`、`log.Writer()`、`log.Flags()`、`log.Prefix()`，调用后以 defer+显式双重路径原样恢复。本注记只更正当时表述，不改写历史判据结果与结论。

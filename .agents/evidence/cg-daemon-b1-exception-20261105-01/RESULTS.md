# RESULTS — cg-daemon-b1-exception-20261105-01

标记：[MARK-CG-B1-EXCEPTION-W67-20261105-01]。worker 定向自检结果；
**不是** verifier 结论，既定红/绿判据与 C 档入口门禁仍由独立 verifier 执行。

## 开工前保全（在源码修改前完成）

- worktree：`/root/workspace/.wt/codegraph-go/fix-cg-sandbox-diag`，分支
  `fix/cg-sandbox-diag`，HEAD `ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5`。
- 五文件现场 SHA256 与任务书登记逐一相符；原字节复制至 `pre-r2/`，
  `pre-r2/SHA256SUMS` 复核一致（见 MANIFEST.md）。

## 修改后的自检（worktree 内，与本次 B1 直接相关）

| 命令 | 结果 |
|---|---|
| `gofmt -l internal/daemon/spawn.go internal/daemon/spawn_test.go` | 空输出，EXIT=0 |
| `go build ./internal/daemon` | EXIT=0（曾一次 `undefined: unix.O_CREATE` 编译错，系本轮自改笔误，已改 `unix.O_CREAT` 重跑通过） |
| `go vet ./internal/daemon` | EXIT=0 |
| `go test -count=1 -timeout 60s -v -run '^TestSpawnDetachedRejectsSymlinkAndNonRegularDaemonLog$' ./internal/daemon` | EXIT=0；6 子例全 PASS：symlink_to_regular_sentinel / symlink_to_oversized_target / dangling_symlink / fifo_leaf / within_directory_dangling_symlink / regular_file_control（0.017s） |
| `go test -count=1 -timeout 300s ./internal/daemon` | EXIT=0；整包 ok（17.257s） |

## 未执行（按任务边界）

- 完整 `go test ./...`、race、CI、静态 check：未执行（worker 自检范围外，
  不冒充独立 verifier）。
- verifier 既定 C 命令的红/绿判据：未执行，由独立 verifier 在新 clone 上跑。
- CodeGraph 入口/MCP 入口启动：未执行。
- commit / merge / push / 部署 / 安装：均未做。

## 实现要点摘要

- `openDaemonLog` 改用单次 `unix.Open(logPath, O_RDWR|O_APPEND|O_CREAT|
  O_NOFOLLOW|O_CLOEXEC, 0600)`：内核 open() 即以 ELOOP 拒绝叶符号链接，
  O_CREAT 之前不落地目标；普通文件经 `f.Stat` 复核；失败关闭自身 fd 返回。
- 移除 os.Root 作用域 open 与打开后 Lstat/SameFile 串（此前那条串会在
  `open 已跟链创建目标之后`才拒绝——同目录悬空软链即凭空创建 rel-target）。
- 新增子例 `within_directory_dangling_symlink`：相对同目录 `rel-target`，
  断言 ELOOP、root/logPath 诊断、目标未创建（标记 CG_B1_INTERNAL_TARGET_CREATED）。

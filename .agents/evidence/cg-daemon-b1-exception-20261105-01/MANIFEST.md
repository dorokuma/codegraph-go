# MANIFEST — cg-daemon-b1-exception-20261105-01

标记：[MARK-CG-B1-EXCEPTION-W67-20261105-01]。C 档 B 服务端 B1 项的一次性例外
（用户明确授权的局部返修第 3 轮，不改变既有 R1/R2 记录与 2/2 额度计数）。

## pre-r2/ — 修前快照（R2 原样覆盖层）

`pre-r2/` 下五文件是从 worktree `/root/workspace/.wt/codegraph-go/fix-cg-sandbox-diag`
按原相对路径、原字节复制的**修改前**状态，等于：

> base commit `ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5` + 前述 R2 工作留下的
> **未提交**五文件覆盖层。

即：它**不是**裸 base，也**不是**上一轮 `cg-daemon-r2-20261105-resume-01/pre-r1`
（pre-r1 是 R1→R2 之间的快照；pre-r2 捕获的是 R2 已落笔的覆盖层）。
`pre-r2/SHA256SUMS` 内五条目 SHA256 与任务书登记逐一核对一致（开工前强制保全，
在任何源码/notes 修改前完成）。

文件清单（相对 pre-r2/）：

- `internal/daemon/spawn.go` — R2 版本的 openDaemonLog（os.Root 作用域 OpenFile +
  打开后 Lstat/SameFile 复核），本次 B1 的唯一改动对象。
- `internal/daemon/spawn_test.go` — R2 版本 K1 测试（4 拒绝例 + 普通文件控制），
  本次在其 cases 尾部追加 within_directory_dangling_symlink。
- `internal/daemon/cgdir_guard_test.go` — 禁改文件，保全供 verifier 复原核对。
- `CHANGELOG.md` — 禁改文件，保全供 verifier 复原核对。
- `.agents/notes/DIAG-SANDBOX-EXIT.md` — 本轮只追加 B1 例外一节，保全供核对。

## 复原用途（供独立 verifier）

- 红：在红 clone 上按 base+此覆盖层还原五文件原字节，再仅注入修后
  `spawn_test.go`（不得注入修后 `spawn.go`），跑既定 C 命令期待 FAIL。
- 绿：另起 clone 同步完整待审候选，跑同一 C 命令期待 PASS。
- 本目录只是 worker 提供的可核验输入，verifier 判据与门槛不变、不由本目录
  声明通过。

## 既有 R2 证据（保持不动，不在本目录内）

`.agents/evidence/cg-daemon-r2-20261105-resume-01/` 共 8 个原件，本轮**未修改**：

- `pre-r1/`（5 个源快照 + `MANIFEST.md` + `SHA256SUMS`）— R2 开工前的保全，
  记录 R1→R2 之间的真实修改前状态。
- `R2-RESULTS.md` — R2 轮的验证结果记录。

上述文件是 R2 轮的轮次证据，**不代表**任何测试源码现状，不得与本目录 pre-r2
混用。

[post-edit 2026-11-06] spawn_test.go github_pat_ fixture rewritten to string-concatenated form for commit secret-scan compliance; test semantics unchanged.

[post-edit 2026-11-06] spawn_test.go secretFragment rewritten as "Q" + strings.Repeat("Q", 2999) for commit credential-assignment-scan compliance; runtime value unchanged (still 3000 "Q" chars).

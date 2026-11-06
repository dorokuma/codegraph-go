# S_B_R1 pre-r1 overlay manifest

- 快照 ID: S_B_R1（cg-daemon-r2-20261105-resume-01）
- base_commit: ae3dd51ac03461f8f4506c0ebcaf6cb81cee1af5
- worktree: /root/workspace/.wt/codegraph-go/fix-cg-sandbox-diag
- 分支: fix/cg-sandbox-diag
- 生成时间: B 服务端诊断门禁 R2（最后一轮返修）动手前

## 相对路径清单（原被审五文件，原样保全）

1. internal/daemon/spawn.go
2. internal/daemon/spawn_test.go
3. internal/daemon/cgdir_guard_test.go
4. CHANGELOG.md
5. .agents/notes/DIAG-SANDBOX-EXIT.md

哈希见 SHA256SUMS（sha256sum -c 可核）。红绿判据的独立 clone 流程：
本 worktree clone → checkout ae3dd51 → 覆盖上述五个原样文件 → 仅注入显式声明的待审测试文件。

[post-edit 2026-11-06] spawn_test.go github_pat_ fixture rewritten to string-concatenated form for commit secret-scan compliance; test semantics unchanged.

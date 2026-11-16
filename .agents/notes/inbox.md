# 观察项收件箱（Inbox）

- `[pattern:pidfile-read-leaf-trust] | type:debt | added:2026-11-10`
  除已收敛的三个探针外，`internal/daemon/lock.go:81,105,160`（`TryAcquireLock`/`ClearStaleLock`/`KillStaleDaemon`）、`cmd/codegraph-go/main.go:101-103`、`internal/daemon/server.go:542`、`internal/db/connection.go:281` 与 `deploy.sh` 存量 pidfile 读点仍直接 `os.ReadFile`/`cat`，后续需按锁竞争与读失败语义区分逐步迁移至 `readRegularPidfile` 叶级 `O_NOFOLLOW` 校验。

- `[pattern:git-assist-subdirectory-path] | type:debt | added:2026-11-13`
  `internal/sync/git_assist.go:29-90` 的 `GitDirtySourceFiles` 将相对仓库根的 `git status` 输出路径直接拼接到调用方传入的 `workdir`，当被索引目录为仓库子目录时拼接路径失真。

- `[pattern:legacy-noncontext-noise-api] | type:debt | added:2026-11-13`
  `internal/extraction/noise.go` 中非 context 导出包装 `ScrubNoisyFailedRefs`（`:108`）与 `ScrubNoisyFailedRefsForFiles`（`:151`）已无生产调用方（生产路径改走 `scrubNoisyFailedRefsForFilesContext`，由 `internal/extraction/orchestrator.go:466` 调用），后续可独立评估清理（注意 `ShouldParkRef` 与 `hasProjectSymbol` 仍由 `index_writer.go:305` 生产路径调用，不得误删）。

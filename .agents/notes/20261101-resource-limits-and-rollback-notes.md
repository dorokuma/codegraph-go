---
status: active
superseded_by: ""
supersedes: ""
模块: "daemon / config / extraction"
---

# 资源限制的边界与旧二进制回退行为（O1 观察项）

## 一句话结论
- 四个资源旋钮是**四层分工**而非互相替代：`index_workers` 限任务并发、`max_procs` 限 Go 调度宽度（不覆盖 CGo OS 线程）、`nice` 降 CFS 调度权重（软让路，空闲不限速）、`cpu_quota_percent` 在 cgroup 层给跨 Go/CGo 全线程的硬带宽上限。旧二进制回退时 `yaml.v3` 静默忽略未知键，不报错。

## 背景
- C 档提交门禁第 1 轮观察项 O1：新增的 `nice` / `max_procs` / `index_workers` / `cpu_quota_percent` 四个资源旋钮各自边界与回滚兼容性需要书面记录，避免后续误配或误判。

## 决策 / 事实记录
- **四层限流分工（互不替代）**：
  1. `index_workers`：限制 AST 抽取任务的并发 worker 池（任务级并行度），由 `internal/extraction` 的 worker 池直接消费；
  2. `max_procs`（`GOMAXPROCS`，钳制 [0,64]）：限制同时执行 Go 代码的 P 数量；但 goroutine 阻塞在 CGo `go-tree-sitter` 调用时占用的 OS 线程（M）**不计入** GOMAXPROCS——它只钉住 Go 侧调度宽度，不能把 AST 提取的 CPU 总量严格限在 N 核；
  3. `nice`（钳制 [0,19]）：`setpriority(2)` 降低 CFS 调度权重，仅在宿主机 CPU 争用时让出算力，空闲时**不限速**。Linux 下 nice 按线程生效，仅 `PRIO_PROCESS,0` 只会 nice 调用线程——实现先做一次进程级调用，再两轮遍历 `/proc/self/task/<tid>` 对每个线程单独 `setpriority`（覆盖遍历期间新克隆的线程，`ESRCH` 忽略），保证 CGo 侧已存在的 OS 线程同步降级；
  4. `cpu_quota_percent`（钳制 [0,6400]，如 `150` = 1.5 核）：经 `systemd-run --scope -p CPUQuota=<N>%` 在 cgroup v2 层对**整个进程树全部线程**提供硬带宽上限，是唯一能把 CGo 线程也钉死的旋钮。
- **systemd-run 失败与时序边界（如实边界，勿夸大）**：
  - `cpu_quota_percent` 仅在 euid=0 + Linux + `/run/systemd/system` 存在 + `systemd-run` 在 PATH 时生效，其余环境（容器、非 systemd 主机、非 root 会话）自动落回普通 spawn；
  - `cmd.Start()` 同步失败（如权限、cgroup v1-only 主机、transient unit policy）立即同调用回退直接 spawn；
  - scope 异步创建失败且在 150ms 观察窗内非零退出、且尚无存活锁持有者（`daemonAliveAt` 读取 pidfile 并经 `IsProcessAlive` 判活；DB 级 flock 互斥由守护进程或直连回退打开索引库时的 `db.Open` / `checkDirectFallbackSafe` 兜底），同样同调用回退直接 spawn；锁竞争输家 0 退出与赢家持锁两种正常路径都不会误触发回退；
  - 若 D-Bus 拥塞导致 >150ms 才异步失败：spawn 已按"正常启动"返回，后台 reaper goroutine 捕获到非零退出只记 `slog.Warn("systemd-run scope exited with error after startup window")`；客户端侧经 `EnsureAndDial` 6 秒拨号超时后安全落回 direct 模式——DB flock + pidfile 兜底防双写，小窗 TOCTOU 下即使偶发双拉起，抢锁输家 0 退出；
  - 未加 `--collect` 的瞬态失败 scope 可能在 systemd 单元列表中短暂残留（`systemctl list-units` 可见），属供排障的无害残留。
- **旧二进制回退**：回退到不含新字段的旧版本时，`gopkg.in/yaml.v3` 默认忽略未知 YAML 键，`nice` / `max_procs` / `index_workers` / `cpu_quota_percent` / `exclude_dirs` 留在 config.yaml 里不会导致旧版本解析失败，只是静默不生效。若新旧版本间存在 schema（索引库结构）差异，由启动时的版本检查自动触发全量重建或恢复备份索引库，不需人工干预。
- **排除规则收窄单一真源**：启动时 workdir 收窄（会话落在被排除子树时 Workdirs 收敛为 `[target]`）只经 `internal/extraction.NarrowExcludedWorkdirs` → `ShouldSkipDirIn` 一条链路；`internal/config` 不再持有手抄规则副本，`config.Config.ExplicitWorkdir` 负责保留"用户显式传了 -workdir"的信息以区分 cwd 回退。

## 被放弃的方案（必填）
- 用 cgroup v2 手动写 cpu.max 替代 systemd-run：放弃——需要守护进程自己管理 cgroup 节点，复杂度和失败面远大于一次性 transient scope，且非 systemd 主机照样要回退。
- 把 `GOMAXPROCS` 当作 CPU 硬上限文档化：放弃——对 CGo 路径是误导性承诺，只能写清楚「调度宽度」语义。
- `spawn` 回退窗口无限等待 systemd-run 结果：放弃——D-Bus 拥塞下会拖死守护进程启动；150ms 窗口 + reaper warn + 客户端拨号超时回退已覆盖慢失败，代价仅是日志而非可用性。

## 来源
- 关联分支：`feat/exclude-dirs-and-cpu-limit`；相关实现 `internal/daemon/spawn.go`、`internal/daemon/limits.go`、`internal/daemon/limits_linux.go`、`internal/config/config.go`、`internal/extraction/skip.go`。

---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: tools # db | extraction | resolution | server | sync | tools
---

# pidfile 诊断探针叶安全读面：本轮分诊的存量读点与观察项留痕

## 一句话结论
- `daemonAliveAt` / `halfDeadDaemonHoldsLock` / `newbornDaemonInStartupGrace` 三个诊断探针已收敛到共享的 `readRegularPidfile`（`O_NOFOLLOW|O_NONBLOCK` + 同 fd 普通文件校验）；其余存量 pidfile 读点属 out-of-scope 未改，另有四条边界观察项登记备查，均不构成阻塞。

## 背景
- `fix/pidfile-safe-open` 分支 C 档首轮审查（reviewer M1/S1 + oracle S1/S3）后，需要把「改了哪里、没改哪里、为什么没改」的分诊结论留档，供后续轮次与独立评估引用。

## 决策

### ① out-of-scope 存量未改读点 `[pattern:pidfile-read-leaf-trust]`
以下 pidfile 读点本轮未动，仍按路径 `os.ReadFile`/`cat` 读取，叶类型信任问题按独立评估处理：
- `internal/daemon/lock.go:81`（`TryAcquireLock` 锁竞争输家回读 `existing` 锁信息）、`:105`（`ClearStaleLock` 复核死进程记录）、`:160`（`KillStaleDaemon` 读待杀对象）——其中 `TryAcquireLock` 涉及锁竞争与读失败语义区分（读失败时 `existing` 置空仍返回 `taken`），改造需单独评估语义等价性；
- `cmd/codegraph-go/main.go:101-103`（`checkDirectFallbackSafe` 启动期回退安全检查裸读 pidfile）；
- `internal/daemon/server.go:542`（守护进程内 `os.ReadFile(d.pidPath)` 读点，`cleanupArtifacts` 清退前复核锁仍属本进程）；
- `internal/db/connection.go:281`（`lockHolderHint` 的 best-effort 提示解析）；
- `deploy.sh`（shell 侧 `cat`/`grep` pidfile 探测多处）。

### ② 观察项（oracle，非缺陷、登记取舍）
- (a) `[pattern:newborn-grace-untrusted-leaf-asymmetry]`：不可信叶（软链/FIFO）下 `newbornDaemonInStartupGrace` 返回 false、invisible-holder 清扫照常进行——与「保守不杀」的一般方向相反（这里不可信 ⇒ 不给宽限 ⇒ 可能杀）。取舍：伪造宽限比漏授宽限更危险（前者可保护恶意 holder），但确实形成不对称，留档供复审。
- (b) `[pattern:pidfile-parent-dir-trust]`：`O_NOFOLLOW` 仅约束最终叶组件，不防 `.codegraph/` 父目录级软链或指向同一内容的硬链接；父目录防线由 `cgdir.Ensure`/`db.Open` 的目录身份复核承担，本读面不重复覆盖。
- (c) `[pattern:pidfile-unbounded-readall]`：`io.ReadAll` 对超大普通文件无独立尺寸上限，与旧 `os.ReadFile` 同语义（无回归、也无新增防护）；pidfile 由本进程写、尺寸受 `EncodeLock` 约束，攻击面仅在叶已被替换为合法普通大文件时放大内存读，登记备查。
- (d) `[pattern:posix-only-check-entry]`：`paths.go` 为 `readRegularPidfile` 引入 `golang.org/x/sys/unix`，与 `spawn.go` 同属 POSIX 平台假设（非 unix 平台整仓构建本就不受支持，见 `internal/db/pin_other.go` 与 `20261109-posix-only-check-entry.md`）。

## 被放弃的方案（必填）
- 方案 A：本轮顺手把 ① 中全部存量读点一并改成 `readRegularPidfile`。放弃理由：超出授权范围；`TryAcquireLock` 等路径的「读失败 vs 无锁」语义需要区分，统一替换有改变行为的风险，留独立评估。
- 方案 B：给 `io.ReadAll` 加自定义尺寸上限。放弃理由：与旧 `os.ReadFile` 语义同构即无回归，本轮目标是把读面收敛到叶安全边界而非新增限制；登记为观察项 (c)。

## 来源
- 关联审查：codegraph-go `fix/pidfile-safe-open` 分支 C 档首轮 reviewer（M1/S1）与 oracle（S1/S3）审查结论。
- 关联改动：`internal/daemon/paths.go`（`readRegularPidfile`）、`proxy.go`/`recovery.go`/`spawn.go`（三探针接入）、`proxy_test.go`（按探针拆分的回归子测试）、CHANGELOG `[Unreleased]` 条目。

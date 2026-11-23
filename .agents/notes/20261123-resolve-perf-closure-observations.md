---
status: active
superseded_by: ""
supersedes: ""
模块: "resolution / db"
---

# fix/resolve-perf 收口观察项（C 档 consider/observation 归档）

## 一句话结论
- `fix/resolve-perf` 合并收口时遗留 5 条非阻断观察项：reviewer consider（`markFailed` 每 ref 一次 `UnresolvedRefAttempts` 点查、hasCandidates 双探测口径、`fw.flush` 取消不清 marks、双查 ctx.Err 设计对称）与 oracle O1/O2（单轮 `resolveScope` 缓存对并发节点写不可见、`fw.marks` 全量驻留内存）。均为正确性无碍的取舍/边界，仅作长期优化与运维参考，不改变本次合并结论。

## 背景
- `fix/resolve-perf` 经 C 档两轮 verifier + reviewer + oracle 复审，`make check` 全绿后收口。门禁遗留项按 SYSTEM.md §4 归入「consider/观察项」记录于此，供后续复查或性能再分析时参考。

## 观察项归档

### R1-consider（reviewer 第一轮）：`hasCandidates` 缓存命中探测与未命中路径语义口径不一致
- **位置**：`internal/resolution/resolver.go`（`hasCandidates`/`candidates` 命中路径 `NodeNameExistsContext` vs 未命中 `hasCandidateContext`）。
- **内容**：命中探测按 `name=?` 全名，未命中探测先 refName 后 `nameTail(refName)` 两查。理论边界：某 `reference_name` 无全名节点但 tail 在数轮前有命中缓存、本轮 tail 节点已删 → 缓存为 true 会把本无候选的 ref 排入 retry，仅多一次无害 `resolveOne`。
- **判定**：低概率多排队一次失败重试，无正确性问题；两处探测语义建议后续统一为同口径（缓存键+探测均按 tail 或均按 refName）以免误推等价。不要修改探测的取消窗口语义。

### R2-consider（reviewer 第一轮，设计确认无需改）：`fw.flush` 取消返回前未清空 `fw.marks`
- **内容**：取消早退不清 marks（`edgeWriter.flush` 的 `w.batch=w.batch[:0]` 取消路径同样跳过，同构模式）；marks 随 scope 生灭无泄漏，仅一致性提示。无实际后果。

### R3-consider（reviewer 第一轮，设计确认非缺陷）：`fw.flush` 双查 `ctx.Err()`
- **内容**：构造 refs 后与发起事务前各查一次，与 `edgeWriter.flush` 双查同构，防「构造期间取消」的对称设计，非冗余。

### R4-consider（reviewer 第二轮）：`markFailed` 的 `UnresolvedRefAttempts` 点查
- **位置**：`internal/resolution/resolver.go` `failureWriter.markFailed`。
- **内容**：为预测 `queuedAbandoned` 对每条失败 ref 做一次 `SELECT attempts`（主键 O(1)），10 万级 backlog 多 ~10 万点查。
- **判定**：「精度换吞吐」可接受取舍；`seen[r.ID]` 去重使同 ref 不重复标记。若 flame 显示占比显著，可改 `UPDATE ... RETURNING` 内联 queued 标志。本轮不得为消点查改签名或放宽统计精度。

### O1（oracle）：单轮 `resolveScope` 缓存对并发节点写不可见
- **位置**：`resolver.go` `resolveScope`（`cand`/`hasCand`/`imported` 三 map 按 pass 首查冻结）。
- **内容**：pass 长跑期间另一 goroutine 写/删同 DB nodes 时，已缓存 name 的后续同名 ref 看不到新候选（漏本轮解析、attempts+1）或拿到已删节点（FK 兜底走 `w.failed`）。`hasCand=false` 负缓存是最大偏移面。后果限于本轮漏解析+多记一次 attempts，下轮自愈，无错边终态；仅「解析 pass 与节点写并发且恰好同名」窄窗口下长期可累积误老化。
- **判定**：把「节点集单轮不变」从事实近似固化为硬假设；若 daemon 解析与节点写互斥可免处理，否则建议 AGENTS.md 登记不变量。观察项而非缺陷。
- **探针规格（供 verifier 竞态放大）**：`ResolveAllContext` 运行中另一连接 `UpsertNode` 同名节点，断言该 ref 本轮 nil plan 且被 markFailed；多轮验证恢复。非单次确定性。

### O2（oracle）：`fw.marks` 全量驻留内存，超大失败 backlog 下单轮堆占用无界
- **位置**：`failureWriter.marks`（无分片/上限，每 `failedMark` ~200-300B）。
- **内容**：67.8 万失败 mark 场景约 150-250MB 常驻至 pass 尾；`failureWriter` 刻意无中刷（中刷无一致性收益）。低内存/`cpu_quota` 受限部署下有 GC/OOM 风险面。
- **判定**：真实取舍待决（本次范围外）；后续可拍板是否分片 flush（如每 10k marks 一事务）。观察项。

## 补充不变量（perf/mega-workspace-scale S1 修复时登记，Oracle O2）
- `ForEachUnresolvedRefsContext` 的流式 `failed` 扫描与 `failureWriter.seen` 构成硬不变量：**凡在同一 pass 内将某 ref 置为 `failed`/`abandoned` 的路径，必须经过 `failureWriter.markFailed` 登记 `fw.seen[r.ID]`**。流式扫描（keyset 分页、边扫边处理）会看到本 pass 刚翻转为 failed 的行并重复处理（双计 attempts、双计 Failed/Abandoned）；旧快照读在 pending 循环之前取 failed 集，天然看不到它们。新增任何同 pass 内标记失败的路径时不得绕过 `markFailed`/`seen`。

## 来源
- 关联分支：`fix/resolve-perf`（合并后删）、`perf/mega-workspace-scale`；实现 `internal/resolution/resolver.go`、`internal/db/query_refs.go`。
- 门禁报告：`role-oracle-79b3347c`、`role-reviewer`（两轮）、`role-verifier`（两轮）。

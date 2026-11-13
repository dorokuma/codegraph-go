---
status: open
scope: shutdown cancellation repair and evidence
---

# Shutdown cancellation review follow-up

[pattern:missing-context-propagation]
[pattern:verification-evidence-provenance]
[pattern:missing-deletion-cancellation]

The Oracle review completed on 2026-11-13 (recovered verdict in `REVIEW-ORACLE.md`; opening scrollback missing). Its S1/S2 follow-up adds a shutdown check before Git-assist probing, context-aware file lists and checks between atomic `ClearFile` transactions. Scratch SQLite deletion triggers now exercise interruption inside the first actual transaction, with remaining-file and normal-path controls; the watcher regression parks that transaction and checks `WaitIdle` drains only its one unit. The single source-bound parent `make check` passed (exit 0, 136.06 seconds); all 24 manifest inputs and both evidence hashes were verified. Changed extraction/resolution/server/sync packages executed anew; some unchanged packages were cached. Evidence is under `.codegraph/shutdown-investigation/oracle-followup-20261113/`. Independent verification remains open; no commit authorization is inferred from WorkBuddy's transferred recommendation.

Reviewer A's 2026-11-12 report (transferred by WorkBuddy) found reachable non-context incremental reference scans, noise symbol lookups and synthesis graph/node reads. Source inspection confirmed these; the uncommitted repair now adds context variants while retaining legacy wrappers and SQL/transaction contracts.

Tests belong in existing `interrupt_test.go`, `resolver_batch_test.go`, `synth_fail_test.go` and DB query tests. Scratch-only SQLite scalar functions/views inject active cancellation without adding production test hooks. Resolution must preserve a previously buffered unflushed plan, and synthesis must preserve the old complete graph when a read inside `classMethods` cancels.

Evidence is distinct from authority: the main agent's `make check` and local fresh-clone rounds do not replace independent verifier/reviewer/Oracle gates. Historical logs bind historical snapshots only. The first repaired live probe failed because it assumed the in-project socket despite the long-path `/tmp` fallback; the corrected probe uses the fixture pidfile's socket path. Preserve both failures and successes.

Verified results and open gates are recorded in `FIX-REPORT.md`. The repaired live MCP run exited naturally after 3.025 seconds, with `integrity=ok pending=19000 meta=[]`. Three main-agent fresh-clone `make check` rounds passed with unique GOCACHEs; their 22 snapshot hashes matched the tree when checked. The 2026-11-13 Oracle follow-up subsequently changes deletion/pruning, the Git-probe call site, tests and wording, so those historical rounds do not bind the new tree. Preserve the preceding failure caused by the probe script injecting GOMAXPROCS=4, which contradicted an existing environment-precedence test; the script now unsets it without changing production behavior or that test. These executions are not independent verifier gates. Independent acceptance remains open; no commit, push, deployment or installed codegraph binary replacement is authorized by this note.

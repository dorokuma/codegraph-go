# Oracle shutdown review

## Verdict and provenance

**Not a clean pass.** The requested `group/oracle` review completed on 2026-11-13. Its report confirms the repaired post-extraction cancellation chain and active-cancellation regressions, identifies two remaining shutdown paths (S1/S2), and keeps independent verification open (O7).

The reviewer was read-only and did not run tests, build a binary or modify the repository. Their attempted `hc send` was denied by the role guard; the parent retrieved the terminal reply and closed the completed reviewer. The retrieved reply contains the findings, test assessment, reviewer reconciliation, limitations, follow-up list and the literal `REVIEW_DONE`. Its beginning is missing from terminal scrollback. This document is a faithful verdict summary, **not a claim that the entire original report was retrieved**. The transferred summary from WorkBuddy supplements the missing S1/S2 headings; the parent checked the relevant source before modifying it.

The review binds the tree examined before the subsequent deletion/Git-probe follow-up. Those follow-up edits require new verification and review; they are not retrospectively approved by this verdict.

## Confirmed improvements

- Incremental reference scans, noise symbol lookups, synthesis graph/node reads and the named candidate paths use context-aware variants. The earlier report's listed gaps are repaired, not still-present defects.
- DB regressions cancel inside SQLite evaluation through scalar functions and scratch views. Resolution cancels while reading the second reference's target, after the first plan is buffered. Synthesis cancels inside `classMethods`' node read after two completed passes. These exercise active work rather than only pre-canceled entry.
- Previously added `shutdown_test.go` files are absent. Assertions are consolidated into existing DB, extraction and resolution test files.
- Legacy non-context APIs, SQL selection/deduplication/limits and the atomic `ReplaceSynthesizedEdges` boundary remain intact.
- The reviewer independently checked the 22-file source manifest and three check-log hashes. The three `make check` executions were still performed by the parent, not an independent verifier.

## Remaining findings

### S1 — Git-assist probe begins before checking shutdown

`internal/server/server.go` called `GitDirtySourceFiles` before its `BgDone` check. Minimal follow-up: check the signal at the call site before starting the probe, retaining the existing public signature and timeout. Making an already-started Git probe context-aware remains deferred.

### S2 — Directory deletion/pruning continues its write backlog

`internal/extraction/orchestrator_file.go` used non-context `ListFiles` and repeated `ClearFile` transactions without shutdown checks. This can keep watcher `WaitIdle` waiting after shutdown. Minimal follow-up: use existing `ListFilesContext(o.indexContext())`, check cancellation between file transactions, return `ErrIndexInterrupted`, and let an already-started `ClearFile` transaction finish atomically. Regression requirements: cancellation during the first real deletion, remaining files preserved, normal deletion control, and bounded watcher draining after the in-flight transaction is released.

### O1/O2 — Transaction contract wording

Cancellation is checked before initiating new work. An already-started `ApplyResolvedRefs` transaction is not context-bound and can finish atomically after the signal. Some references may therefore resolve while the rest remain pending; a decreasing pending count is not evidence of reference loss. The earlier “finish an already-started transaction” comment was placed before transaction creation and should state the actual mechanism accurately.

## Deferred observations

- **O3:** `ScrubNoisyFailedRefs`' full non-context variant remains, but the reviewer found no production caller; the orchestrator calls the context-aware file-scoped variant.
- **O4:** Cancellation during an initial SQLite step can surface an SQLite `interrupted` error rather than `context.Canceled`. The reviewed callers check `ctx.Err()` before aging/persisting failure, so this is an error-identity/observability follow-up.
- **O5:** Existing Git-assist root-relative path handling can be wrong when the workdir is a repository subdirectory. This is outside the shutdown diff.
- **O6:** The live MCP fixture covers primary-workdir full rebuild/full resolution, not every incremental, inner-synthesis, multi-workdir or slow-Git shape.
- **O7:** Independent three-round fresh-clone verification remains open. Parent checks and Oracle hash verification do not replace independent verifier execution.

## Evidence limits

The live run exited naturally in 3.025 seconds after disconnect and retained `integrity=ok pending=19000 meta=[]`; the socket/pidfile were removed. It is a single parent-run workload. The first failed socket-assumption probe and the check-series failure caused by injected `GOMAXPROCS=4` are retained. Three corrected parent-run fresh-clone `make check` rounds passed; they bind the pre-follow-up snapshot only.

The review excluded the separate MCP argument-unwrapping changes. No codegraph commit, push or installed binary replacement is authorized by this report.

## Completion marker

The original retrieved reviewer reply ended with:

```
REVIEW_DONE
```

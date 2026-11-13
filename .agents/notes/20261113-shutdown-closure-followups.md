---
status: open
scope: shutdown cancellation closure follow-ups and protocol gaps
---

# Shutdown closure follow-ups

## Deferred Oracle observations (not fixed this round)

- O3 [pattern:legacy-noncontext-noise-api]: the full non-context noise API (`internal/extraction/noise.go` legacy wrapper) is still present and unused; scope is noise symbol lookup paths only. Not fixed this round.
- O4 [pattern:sqlite-cancellation-error-identity]: the initial SQLite step can return SQLite interrupted rather than `context.Canceled`; callers check `ctx.Err` before aging, so observability only. Scope is the initial `sqlite3_step` interruption point in DB query paths. Not fixed this round.
- O5 [pattern:git-assist-subdirectory-path]: the existing Git-assist probe (`internal/sync/git_assist.go:29-90`, `GitDirtySourceFiles`) joins git-status output paths — repo-root-relative — onto the caller-supplied workdir, so paths are wrong when the indexed workdir is a repository subdirectory. Scope is that path joining only; the separate `internal/server` call-site early-exit already landed. Not fixed this round.
- O6 [pattern:live-verification-coverage]: the live fixture exercised only a single full primary-workdir rebuild + resolution; incremental/inner-synthesis interruption is covered by regression tests only, not live. Scope is live-verification coverage expansion. Not fixed this round.

## Verification protocol gaps (independent run 2026-11-13)

- [pattern:missing-origin-removal]: `run.sh` did not remove the clone's `origin` remote before verification — a clone-protocol deviation from a fully isolated clone (recorded, not concealed). The clone used `--no-hardlinks`, had its own `.git` metadata and a per-round unique `GOCACHE`, and the pre-run manifest matched the frozen snapshot, so the hash evidence itself still stands; missing `origin` removal does not by itself prove shared writable git objects. Scope is the verification harness script.
- [pattern:missing-gate-scan]: this main session did not obtain a usable `gate_scan` tool and did not receive a wb scan artifact; whether wb scanned anything during their run is unverified. The closure gate therefore lacks that scan artifact and remains open — no gate-passed claim is made. Scope is the closure-gate tooling.

Both protocol gaps are recorded in `FIX-REPORT.md` under "Protocol gaps still open". §4/§5 acceptance requirements are not fully satisfied; updated acceptance criteria await a focused review pass. No commit, merge, push, or installed-binary replacement is authorized by this note.

## Updates 2026-11-13 (verifier r4 + S2 evidence)

- Supersedes the `[pattern:missing-origin-removal]` open statement above: wb `run4.sh` (SHA-256 `b7f7e1345dc8bfd2d311f066511c16c93ce3289ab92d413cda55f2597e87f117`) removed the clone's `origin` before `make check` (`remotes_after_remove=0`) and added post-run manifest re-verification (`manifest_before=identical manifest_after=identical`) plus unchanged source refs. Round 4 `make check` rc=0, 371s, 12 pkgs, 0 cached/0 skip/0 fail; `make-check.log` SHA-256 `250ddd99277d322fc62145b98c7f1f359688459e3f108f2cb898b225623d76dd`. Full details in `FIX-REPORT.md` §"Verifier round 4". The r1–r3 runs remain valid hash evidence but their clone-isolation deviation is closed by r4. `[pattern:missing-gate-scan]` is NOT superseded: no `gate_scan` artifact was obtained; a wb-side proxy scan (Test counts / `t.Skip` / build-ignore-only files) was reported but not independently re-verified and is not equivalent to a full gate scan — degrade to manual review per SYSTEM §3, no gate-passed claim.
- [pattern:git-assist-preprobe-test-gap]: S1 (Git-assist pre-probe `BgDone` check at `internal/server/server.go:405-413`) has NO dedicated regression test — the call-site early exit landed without a test that closes `BgDone` before `GitDirtySourceFiles` runs. S2 evidence actually covers only the three file-deletion subtests `TestFileDeletionStopsBetweenTransactions/{tree,root,prune}` (green PASS / red FAIL, `delete calls=6 err=<nil>, want one committed unit then interruption`; logs under `/root/wb-s2-20261113/logs/`, see `FIX-REPORT.md` §"S2 red/green evidence"). Watcher cancellation paths rely on the pre-existing full `make check -race` runs with no red counterpart. No test-coverage claim is made for S1; this is a registered residual gap, not acceptance granted.

## Focused acceptance record (2026-11-13, reviewer t8z)

- Reviewer t8z issued the updated acceptance on the read-only main tree and checked all transaction/resource/code/test obligations: seven unit changes plus all seven production call sites were verified; no code must/should items remain, no implementation rework required. Scope of acceptance: S1/S2/O1/O2. The Oracle's original context-propagation finding stands; S1/S2/O1/O2 follow-ups are accepted. O3–O6 remain out of scope per this note's deferred section.
- Registered residuals accepted by the reviewer/user as recorded gaps: S1's missing dedicated regression test and the watcher's absence of a red counterpart are explicitly user-registered leftovers. The S2 rsync green-before-red protocol deviation (no git metadata, no five-case wrapper claim) is accepted per the user's explicit "direct reference, no rerun needed" — it stands as failure-capable control evidence, not a claim of the original wrapper execution.
- `[pattern:missing-gate-scan]` remains open: `gate_scan` tooling was not obtained this session; classification was degraded to manual item-by-item review per SYSTEM §3.87. No confirmed/unresolved must/should items were found manually; manual review is not presented as a tool invocation.
- Four independent `make check` rounds (wb r1–r4) all exited 0; the current tree matches the frozen manifest. Branch preservation phase commits split by concern (shutdown fix vs MCP arg-unwrap); main merge is a separate authorized task. [pattern:closure-record]

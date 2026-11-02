//go:build linux

package daemon

import (
	"os"
	"strconv"
	"syscall"
)

// setNiceRounds bounds the /proc/self/task scan below. Threads normally
// settle within the first pass or two; the bound only exists so a pathological
// workload that keeps cloning threads can never turn the loop into an
// unbounded spin at process startup.
const setNiceRounds = 8

// setNice lowers the scheduling priority of this process to prio (0-19).
// Linux schedulers apply nice per thread, not per process: threads created
// before the call (or cloned while we iterate) keep their old priority, so
// a plain PRIO_PROCESS/0 call only reaches the calling thread. This matters
// for CGo-heavy workloads — tree-sitter parsers burn CPU on OS threads the
// Go scheduler spawned, and every one of them must be niced for the limit
// to mean anything on a contended host.
//
// Strategy: first the process-wide call (covers the calling thread and
// anything inheriting later), then a BOUNDED fixed-point walk of
// /proc/self/task: each round re-reads the task directory and applies
// setpriority to every tid not seen yet, and the walk stops as soon as a
// round finds no new tid (converged) — at the latest after setNiceRounds
// rounds. A tid that exits between ReadDir and Setpriority reports ESRCH,
// which is ignored. When /proc/self/task is unreadable (no /proc mounted)
// the first call's result is returned as the best effort.
//
// Honest scope: this is best-effort convergence, not a guarantee. A thread
// cloned AFTER the final round inherits the priority of the thread that
// spawned it (usually already niced, so correct in practice), but a CGo
// library that clones from a thread whose nice value could not be raised
// (no CAP_SYS_NICE, lowering only) can still slip through; so can any thread
// that appears after the round bound is hit. For a hard ceiling on total CPU
// use the cgroup-level CPUQuota scope (cpu_quota_percent) is the layer that
// covers every thread of the process tree by construction — nice only
// reweights CFS scheduling under contention.
//
// Only a nice level higher than the current one is achievable without
// CAP_SYS_NICE; errors are reported to the caller for logging.
func setNice(prio int) error {
	err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, prio)

	seen := make(map[int]struct{})
	for round := 0; round < setNiceRounds; round++ {
		entries, readErr := os.ReadDir("/proc/self/task")
		if readErr != nil {
			return err
		}
		fresh := 0
		for _, e := range entries {
			tid, convErr := strconv.Atoi(e.Name())
			if convErr != nil || tid <= 0 {
				continue
			}
			if _, done := seen[tid]; done {
				continue
			}
			seen[tid] = struct{}{}
			fresh++
			if sErr := syscall.Setpriority(syscall.PRIO_PROCESS, tid, prio); sErr != nil && sErr != syscall.ESRCH {
				if err == nil {
					err = sErr
				}
			}
		}
		// Fixed point: this round saw no thread the previous rounds had not
		// already handled, so further scans can only repeat the same work.
		if fresh == 0 {
			break
		}
	}
	return err
}

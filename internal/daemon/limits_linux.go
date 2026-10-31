//go:build linux

package daemon

import (
	"os"
	"strconv"
	"syscall"
)

// setNice lowers the scheduling priority of this process to prio (0-19).
// Linux schedulers apply nice per thread, not per process: threads created
// before the call (or cloned while we iterate) keep their old priority, so
// a plain PRIO_PROCESS/0 call only reaches the calling thread. This matters
// for CGo-heavy workloads — tree-sitter parsers burn CPU on OS threads the
// Go scheduler spawned, and every one of them must be niced for the limit
// to mean anything on a contended host.
//
// Strategy: first the process-wide call (covers the calling thread and
// anything inheriting later), then walk /proc/self/task and setpriority
// each tid explicitly. Two passes over the task directory shrink the race
// window where a thread is cloned mid-iteration; a tid that exits between
// ReadDir and Setpriority reports ESRCH, which is ignored. When
// /proc/self/task is unreadable (no /proc mounted) the first call's result
// is returned as the best effort.
//
// Only a nice level higher than the current one is achievable without
// CAP_SYS_NICE; errors are reported to the caller for logging.
func setNice(prio int) error {
	err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, prio)

	entries, readErr := os.ReadDir("/proc/self/task")
	if readErr != nil {
		return err
	}
	// Pass 2 catches threads cloned while pass 1 ran; anything cloned even
	// later inherits the process-group priority set by the first call above
	// in practice (children of a niced thread inherit its nice value), so
	// two bounded passes are enough without looping until fixpoint.
	for pass := 0; pass < 2; pass++ {
		for _, e := range entries {
			tid, convErr := strconv.Atoi(e.Name())
			if convErr != nil || tid <= 0 {
				continue
			}
			if sErr := syscall.Setpriority(syscall.PRIO_PROCESS, tid, prio); sErr != nil && sErr != syscall.ESRCH {
				if err == nil {
					err = sErr
				}
			}
		}
		if pass == 0 {
			if entries, readErr = os.ReadDir("/proc/self/task"); readErr != nil {
				break
			}
		}
	}
	return err
}

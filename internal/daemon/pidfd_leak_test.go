package daemon

import (
	"os"
	"runtime"
	"testing"
)

// TestIsProcessAliveNoFdLeak guards the pidfd leak regression: on Go 1.24+
// Linux, os.FindProcess opens a pidfd per call and the old IsProcessAlive
// implementation never released it, so the PPID watchdog (one probe per
// PPIDPollInterval tick, default 5s) leaked one fd per tick per process —
// unbounded growth (measured 96→99 per 15s on a live client). The kill(2)
// signal-0 probe allocates zero fds.
//
// Linux only (/proc/self/fd is the fd census; the pidfd leak itself is a
// Go-on-Linux behavior). The before/after counts tolerate a small ambient
// fluctuation (+2) but a per-call fd allocation would blow past it by
// ~200×.
func TestIsProcessAliveNoFdLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc/self/fd")
	}
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatalf("list /proc/self/fd: %v", err)
		}
		return len(entries)
	}
	const calls = 200
	before := countFDs()
	for i := 0; i < calls; i++ {
		if !IsProcessAlive(os.Getpid()) {
			t.Fatal("self should be alive")
		}
	}
	after := countFDs()
	if delta := after - before; delta > 2 {
		t.Fatalf("fd count grew by %d over %d IsProcessAlive calls (pidfd leak?); before=%d after=%d", delta, calls, before, after)
	}
}

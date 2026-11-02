//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestApplyProcessLimitsLinuxAllThreads is the regression test for the
// Linux per-thread nice gap: setpriority(PRIO_PROCESS, 0) only nices the
// calling thread on Linux, so a plain call would leave every OS thread the
// Go scheduler already spawned (e.g. CGo-blocked tree-sitter workers) at
// the old priority. The real assertion runs in a re-executed child so the
// test binary's own priority is never touched.
func TestApplyProcessLimitsLinuxAllThreads(t *testing.T) {
	if os.Getenv("CODEGRAPH_LIMITS_LINUX_CHILD") == "1" {
		limitsLinuxChildMain(t)
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestApplyProcessLimitsLinuxAllThreads$")
	child.Env = append(os.Environ(), "CODEGRAPH_LIMITS_LINUX_CHILD=1")
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("limits child failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("limits child reported failure:\n%s", out)
	}
}

// limitsLinuxChildMain runs inside the re-executed test binary. It pins
// several goroutines to their own OS threads, applies the nice level, then
// reads /proc/self/task/<tid>/stat and asserts EVERY thread of the process
// carries the new nice value (field 19 of stat, 0-based index 16 after
// splitting on the last ')' that ends the comm field).
func limitsLinuxChildMain(t *testing.T) {
	// Lock several goroutines to their own OS threads so /proc/self/task
	// has multiple entries before ApplyProcessLimits runs — this is what
	// makes the test meaningful (a single-threaded process cannot expose
	// the PRIO_PROCESS/0 gap).
	const pinned = 4
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < pinned; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			<-release
			runtime.UnlockOSThread()
		}()
	}
	// Give the locked goroutines a moment to materialize as threads.
	time.Sleep(100 * time.Millisecond)

	// nice=12 / maxProcs=0 (GOMAXPROCS untouched): every OS thread must
	// carry the new nice value afterwards.
	ApplyProcessLimits(12, 0)

	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		close(release)
		wg.Wait()
		t.Fatalf("read /proc/self/task: %v", err)
	}
	threads := 0
	for _, e := range entries {
		tid, convErr := strconv.Atoi(e.Name())
		if convErr != nil || tid <= 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc/self/task", e.Name(), "stat"))
		if err != nil {
			continue // thread may have exited between ReadDir and ReadFile
		}
		nice := niceFromProcStat(string(stat))
		if nice < 0 {
			close(release)
			wg.Wait()
			t.Fatalf("unparsable stat for tid %d: %q", tid, stat)
		}
		if nice != 12 {
			close(release)
			wg.Wait()
			t.Fatalf("tid %d has nice=%d, want 12 — PRIO_PROCESS/0 only niced the calling thread", tid, nice)
		}
		threads++
	}
	close(release)
	wg.Wait()
	if threads < 5 {
		t.Fatalf("expected ≥5 threads (4 locked + main), saw %d", threads)
	}
}

// niceFromProcStat extracts the nice value (field 19) from
// /proc/<tid>/stat content. The comm field "(...)" may itself contain
// spaces and parens, so fields are counted from the last ')' — nice is at
// 0-based index 16 of the remainder.
func niceFromProcStat(stat string) int {
	i := strings.LastIndex(stat, ")")
	if i < 0 || i+2 > len(stat) {
		return -1
	}
	fields := strings.Fields(stat[i+2:])
	if len(fields) <= 16 {
		return -1
	}
	n, err := strconv.Atoi(fields[16])
	if err != nil {
		return -1
	}
	return n
}

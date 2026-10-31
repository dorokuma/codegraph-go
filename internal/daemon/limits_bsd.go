//go:build unix && !linux

package daemon

import "syscall"

// setNice lowers the scheduling priority of this process to prio (0-19).
// On BSD/Darwin PRIO_PROCESS with who=0 applies the priority to the whole
// process group entry the way callers expect; per-thread iteration like the
// Linux /proc/self/task walk is unnecessary here. Only a nice level higher
// than the current one is achievable without elevated privileges; errors
// are reported to the caller for logging.
func setNice(prio int) error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, prio)
}

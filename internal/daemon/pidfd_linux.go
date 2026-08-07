//go:build linux

package daemon

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdOpen pins pid with pidfd_open(2) (Linux 5.3+). The raw error is
// returned untouched so terminateViaPidfd can classify it: ENOSYS (kernel
// without the syscall), EPERM (ptrace/seccomp restrictions), ESRCH (dead
// pid) all become ErrPidfdNotSupported → FindProcess fallback.
func pidfdOpen(pid int) (int, error) {
	return unix.PidfdOpen(pid, 0)
}

// pidfdSendSignal delivers sig through the pidfd (pidfd_send_signal(2),
// Linux 5.1+): the signal goes to the pinned process incarnation even if its
// pid was recycled. ESRCH means the pinned process is gone.
func pidfdSendSignal(fd int, sig syscall.Signal) error {
	return unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0)
}

// pidfdClose releases the pin. Closing the pidfd never affects the pinned
// process itself.
func pidfdClose(fd int) error {
	return unix.Close(fd)
}

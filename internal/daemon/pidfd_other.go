//go:build !linux

package daemon

import "syscall"

// pidfd_open(2)/pidfd_send_signal(2) are Linux-only. On every other platform
// the pidfd path reports ErrPidfdNotSupported so terminateInvisibleHolder
// uses the classic os.FindProcess + Signal + live-recheck fallback (the
// pre-signal /proc rechecks carry the identity guarantee there).
func pidfdOpen(pid int) (int, error) {
	return -1, ErrPidfdNotSupported
}

// pidfdSendSignal is never reached on non-Linux (pidfdOpen fails first);
// returning ENOSYS keeps the interface honest if it ever is.
func pidfdSendSignal(fd int, sig syscall.Signal) error {
	return syscall.ENOSYS
}

func pidfdClose(fd int) error {
	return nil
}

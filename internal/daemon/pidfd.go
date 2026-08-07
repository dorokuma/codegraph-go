package daemon

import (
	"errors"
	"fmt"
	"log"
	"syscall"
	"time"
)

// ErrPidfdNotSupported is returned by terminateViaPidfd when the platform or
// kernel cannot pin a process with a pidfd: non-Linux build, pidfd_open(2)
// missing (ENOSYS on kernels < 5.3), or an open failure for another reason
// (EPERM under ptrace/seccomp restrictions, ESRCH for a dead pid, ...). The
// caller (terminateInvisibleHolder) must fall back to the classic
// kill(2) + live-recheck path: the pidfd path closes the pid-reuse signal
// window, it is not a correctness requirement.
var ErrPidfdNotSupported = errors.New("pidfd not supported")

// pidfdOpenFn / pidfdSendSignalFn / pidfdCloseFn are the pidfd primitives
// used by terminateViaPidfd. They are variables (signalFn style) so tests
// can force the fallback path (open returns ErrPidfdNotSupported) or
// simulate a survivor (send no-ops + waitForExitFn false) without relying on
// kernel support. Production always uses the real Linux implementations from
// pidfd_linux.go (or the not-supported stubs from pidfd_other.go).
var (
	pidfdOpenFn       = pidfdOpen
	pidfdSendSignalFn = pidfdSendSignal
	pidfdCloseFn      = pidfdClose
)

// terminateViaPidfd SIGTERMs pid and escalates to SIGKILL, with the signal
// target PINNED by a pidfd. This closes the snapshot→signal TOCTOU window
// that the FindProcess fallback cannot fully close:
//
// WINDOW-CLOSING PRINCIPLE: pidfd_open(2) returns a file descriptor that
// refers to ONE specific process incarnation. From that moment on, even if
// the pid is recycled by an unrelated process, pidfd_send_signal(2) delivers
// the signal to the pinned process — the original holder — or fails with
// ESRCH once it is gone. The live /proc recheck is still performed before
// every signal (same conservative identity rule as the fallback path), but
// the remaining window between recheck and signal can no longer redirect the
// signal to an innocent process: the pidfd pins the target.
//
// FALLBACK CONDITION: any pidfd_open failure (ErrPidfdNotSupported) makes
// the caller fall back to the classic path. A signal error that is NOT ESRCH
// is returned as-is; ESRCH (the pinned process exited between the recheck
// and the signal, or during the SIGKILL escalation) is a safe no-op.
//
// Returns (true, nil) when the process exited, (false, nil) when the live
// recheck no longer matches (skipped, not counted), and an error only when
// signaling failed or the process survives even SIGKILL (wraps
// ErrInvisibleHolderSurvived — the flock is still held, see that sentinel).
func terminateViaPidfd(root string, pid int) (bool, error) {
	fd, err := pidfdOpenFn(pid)
	if err != nil {
		return false, fmt.Errorf("%w: pid %d: %v", ErrPidfdNotSupported, pid, err)
	}
	defer pidfdCloseFn(fd) //nolint:errcheck

	if !liveInvisibleHolderMatch(root, pid) {
		log.Printf("pid %d no longer matches the invisible-holder profile (died/recycled since scan); skipping", pid)
		return false, nil
	}
	if err := pidfdSendSignalFn(fd, syscall.SIGTERM); err != nil {
		// The pinned process exited between the recheck and the signal.
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	if waitForExitFn(pid, 5*time.Second) {
		return true, nil
	}
	log.Printf("invisible holder pid=%d did not exit within 5s of SIGTERM; sending SIGKILL", pid)
	if !liveInvisibleHolderMatch(root, pid) {
		log.Printf("pid %d no longer matches the invisible-holder profile after SIGTERM grace; skipping SIGKILL", pid)
		return false, nil
	}
	// Same pidfd → SIGKILL targets the SAME process that got SIGTERM, even
	// if its pid was recycled during the grace window. ESRCH means the
	// pinned process exited meanwhile — a safe no-op (the flock is released).
	if err := pidfdSendSignalFn(fd, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, err
	}
	if !waitForExitFn(pid, 2*time.Second) {
		return false, fmt.Errorf("%w: pid %d still alive after SIGTERM+SIGKILL; flock not released", ErrInvisibleHolderSurvived, pid)
	}
	return true, nil
}

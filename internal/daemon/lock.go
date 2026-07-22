package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// AcquireResult is the outcome of TryAcquireLock.
type AcquireResult struct {
	// Kind is "acquired" or "taken".
	Kind     string
	PidPath  string
	Info     LockInfo  // set when Kind=="acquired" (what we wrote)
	Existing *LockInfo // set when Kind=="taken" (may be nil if corrupt)
}

// TryAcquireLock exclusively creates the daemon pidfile with a full record.
// Uses temp+link (atomic, no empty-file window); falls back to O_EXCL open
// when the filesystem has no hard links.
func TryAcquireLock(projectRoot string) (AcquireResult, error) {
	pidPath := PidPath(projectRoot)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		return AcquireResult{}, err
	}

	info := LockInfo{
		PID:        os.Getpid(),
		Version:    PackageVersion,
		SocketPath: PreferredSocket(projectRoot),
		StartedAt:  time.Now().UnixMilli(),
	}

	tmp := pidPath + "." + itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(tmp, EncodeLock(info), 0o600); err != nil {
		return AcquireResult{}, err
	}
	defer os.Remove(tmp) //nolint:errcheck

	acquired := false
	if err := os.Link(tmp, pidPath); err == nil {
		acquired = true
	} else if errors.Is(err, os.ErrExist) || isEExist(err) {
		// lost race
	} else {
		// no hard links or other FS limit — O_EXCL fallback
		ok, ferr := acquireViaExclusiveOpen(pidPath, info)
		if ferr != nil {
			return AcquireResult{}, ferr
		}
		acquired = ok
	}

	if acquired {
		return AcquireResult{Kind: "acquired", PidPath: pidPath, Info: info}, nil
	}

	var existing *LockInfo
	if raw, err := os.ReadFile(pidPath); err == nil {
		existing = DecodeLock(raw)
	}
	return AcquireResult{Kind: "taken", PidPath: pidPath, Existing: existing}, nil
}

func acquireViaExclusiveOpen(pidPath string, info LockInfo) (bool, error) {
	f, err := os.OpenFile(pidPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) || isEExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	if _, err := f.Write(EncodeLock(info)); err != nil {
		return false, err
	}
	return true, nil
}

// ClearStaleLock removes a pidfile only if it still names a dead process.
// expectedDeadPID, when >0, must still match the file contents.
func ClearStaleLock(pidPath string, expectedDeadPID int) bool {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	info := DecodeLock(raw)
	if info != nil {
		if expectedDeadPID > 0 && info.PID != expectedDeadPID {
			return false
		}
		if info.PID > 0 && IsProcessAlive(info.PID) {
			return false
		}
	}
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return false
	}
	return true
}

// IsProcessAlive probes pid with signal 0. EPERM counts as alive.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) {
		return false
	}
	// ESRCH → dead; EPERM → alive but not ours
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if errno == syscall.ESRCH {
			return false
		}
		if errno == syscall.EPERM {
			return true
		}
	}
	// Go on Linux often wraps as "os: process already finished"
	if err.Error() == "os: process already finished" {
		return false
	}
	// Unknown error: be conservative (treat as alive) so we never steal a lock.
	return true
}

// RewriteLock atomically replaces the pidfile body (holder must own the lock).
func RewriteLock(pidPath string, info LockInfo) error {
	tmp := pidPath + "." + itoa(os.Getpid()) + ".relocate"
	if err := os.WriteFile(tmp, EncodeLock(info), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pidPath)
}

func isEExist(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.EEXIST
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

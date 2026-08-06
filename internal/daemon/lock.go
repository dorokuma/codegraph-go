package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		ProcStart:  procStartTime(os.Getpid()),
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

// KillStaleDaemon terminates a live daemon whose version no longer matches
// (B1: version-mismatch cleanup before spawning a fresh daemon). It reads the
// pidfile, verifies the target is really the daemon we recorded (S3: PID-reuse
// guard), SIGTERMs it when alive, polls up to 5s for exit, then clears the
// stale pidfile. ClearStaleLock's expectedDeadPID guard ensures we never
// remove a pidfile that was rewritten to name a different process.
func KillStaleDaemon(projectRoot string) error {
	pidPath := PidPath(projectRoot)
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	info := DecodeLock(raw)
	if info == nil || info.PID <= 0 {
		// Corrupt/empty pidfile — nothing to signal; clear if stale.
		ClearStaleLock(pidPath, 0)
		return nil
	}
	if IsProcessAlive(info.PID) {
		// S3: PID-reuse window — the pid may have died since the pidfile was
		// written and been recycled by an unrelated process. Never signal a
		// process that is not the daemon we recorded: on mismatch, return a
		// clear error without signaling and without touching the lock.
		if err := verifyDaemonIdentity(info); err != nil {
			return err
		}
		proc, ferr := os.FindProcess(info.PID)
		if ferr != nil {
			return ferr
		}
		log.Printf("killing stale daemon pid=%d (version mismatch; upgrade cleanup)", info.PID)
		if serr := proc.Signal(syscall.SIGTERM); serr != nil {
			// The process may have died between the probe and the signal.
			if !IsProcessAlive(info.PID) {
				ClearStaleLock(pidPath, info.PID)
				return nil
			}
			return serr
		}
		// Poll for exit (daemon Stop drains sessions, checkpoints WAL, closes DB).
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			if !IsProcessAlive(info.PID) {
				break
			}
		}
	}
	ClearStaleLock(pidPath, info.PID)
	return nil
}

// procStartTimeFn/procCmdlineFn are the /proc readers used by
// verifyDaemonIdentity. They are variables so tests can simulate an
// unreadable /proc; production behavior always uses the real implementations.
var (
	procStartTimeFn = procStartTime
	procCmdlineFn   = procCmdline
)

// verifyDaemonIdentity guards the SIGTERM in KillStaleDaemon against PID
// reuse: between reading the pidfile and signaling, the recorded pid may have
// died and been recycled by an unrelated process. When /proc is available the
// target must actually be the daemon we recorded — either the /proc start
// time matches the pidfile record (strong check, for pidfiles written with
// ProcStart) or, for older pidfiles without it, the command line must name a
// codegraph process. Platforms without /proc cannot verify and degrade to the
// historical probe-and-signal behavior. Returns nil when the process passes
// verification or verification is impossible.
func verifyDaemonIdentity(info *LockInfo) error {
	if info.ProcStart > 0 {
		cur := procStartTimeFn(info.PID)
		if cur == 0 {
			// The pidfile records a start time but /proc no longer reports
			// one: data is insufficient to confirm identity, so refuse rather
			// than degrade to a weaker check (never risk SIGTERMing a process
			// we cannot identify).
			return fmt.Errorf("refusing to kill pid %d: cannot read process start time (pid reused by another process?)", info.PID)
		}
		if cur != info.ProcStart {
			return fmt.Errorf("refusing to kill pid %d: process start time changed (pid reused by another process)", info.PID)
		}
		return nil
	}
	if cmdline := procCmdlineFn(info.PID); cmdline != "" {
		if !isCodegraphCmdline(cmdline) {
			return fmt.Errorf("refusing to kill pid %d: not a codegraph daemon (cmdline %q; pid reused by another process)", info.PID, cmdline)
		}
		return nil
	}
	// No /proc on this platform: cannot verify — keep historical behavior.
	return nil
}

// isCodegraphCmdline reports whether the process argv names a codegraph
// daemon binary. Only argv[0] (the executable path) is inspected, and only
// via exact basename/path-segment matching — never a bare substring match,
// which would let an unrelated process whose path merely contains
// "codegraph" (e.g. an editor under .../codegraph-editor/) pass the check
// and get SIGTERMed. argv[0] may be relative or absolute and may contain
// spaces (a path with spaces); the basename comparison handles both.
func isCodegraphCmdline(cmdline string) bool {
	argv0 := cmdline
	if i := strings.IndexByte(cmdline, 0); i >= 0 {
		argv0 = cmdline[:i]
	}
	if argv0 == "" {
		return false
	}
	switch filepath.Base(argv0) {
	case "codegraph", "codegraph-go":
		return true
	}
	// Path-segment fallback for relative or cleaned paths: a segment equal to
	// "codegraph-go", or a path ending in "codegraph" or "codegraph-go".
	cleaned := filepath.Clean(argv0)
	for _, seg := range strings.Split(cleaned, string(filepath.Separator)) {
		if seg == "codegraph-go" {
			return true
		}
	}
	return cleaned == "codegraph" || strings.HasSuffix(cleaned, "/codegraph") || strings.HasSuffix(cleaned, "/codegraph-go")
}

// procStartTime returns the starttime field (22nd) of /proc/<pid>/stat, or 0
// when unavailable (non-procfs platforms, permission, vanished process).
// starttime counts clock ticks since boot and uniquely identifies a process
// incarnation, so comparing it detects PID reuse.
func procStartTime(pid int) int64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// comm (field 2) may contain spaces/parens; fields restart after the last ')',
	// where field 3 (state) becomes index 0. starttime is field 22 → index 19.
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 || i+2 >= len(raw) {
		return 0
	}
	fields := strings.Fields(string(raw[i+2:]))
	if len(fields) < 20 {
		return 0
	}
	v, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// procCmdline returns the NUL-joined argv of pid from /proc, or "" when
// unavailable.
func procCmdline(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return string(raw)
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

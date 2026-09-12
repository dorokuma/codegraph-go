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

// ErrStaleDaemonRefused is wrapped by every KillStaleDaemon error that REFUSES
// to terminate the recorded process because its identity could not be
// confirmed against /proc (PID-reuse guard: start time unreadable or changed,
// exe/environ identity facts missing or mismatched, or a legacy pidfile
// without a recorded start time that cannot be checked for PID reuse at all).
// Callers must treat it as fatal
// for the daemon path: neither spawn a replacement nor fall back to direct
// mode while an unidentified live process holds the lock — both would risk
// double-writing the index. Transient kill failures (e.g. signal sent but
// exit wait timed out) do NOT wrap this sentinel and may be retried via a
// fresh spawn.
var ErrStaleDaemonRefused = errors.New("refusing to kill: identity check failed (pid reused by another process)")

// KillStaleDaemon terminates a live daemon whose version no longer matches
// (B1: version-mismatch cleanup before spawning a fresh daemon). It reads the
// pidfile, verifies the target is really the daemon we recorded (S3: PID-reuse
// guard + identity bound to /proc facts outside the pidfile — see
// verifyDaemonIdentity), SIGTERMs it when alive, polls up to 5s for exit, then
// escalates to
// SIGKILL when the grace expires (a daemon stuck in Stop must not keep the
// lock, and returning a fake nil here would make the caller spawn a
// replacement doomed to fail on the still-held lock). Identity is re-verified
// immediately before the SIGKILL: the grace window is a PID-reuse TOCTOU —
// the daemon may have exited right after SIGTERM and its pid been recycled by
// an unrelated process, so signaling without a recheck could kill an innocent
// process (same pre-signal recheck discipline as terminateViaKill in
// recovery.go). If the process survives even SIGKILL, an explicit error is
// returned and the stale pidfile is left in place (ClearStaleLock never
// removes a live pidfile).
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
		log.Printf("killing stale daemon pid=%d (version mismatch; upgrade cleanup)", info.PID)
		// kill(2) directly — same recheck discipline as before, but without
		// os.FindProcess, which would allocate an unreleased pidfd per call
		// on Go 1.24+ Linux.
		if serr := syscall.Kill(info.PID, syscall.SIGTERM); serr != nil {
			// The process may have died between the probe and the signal.
			if !IsProcessAlive(info.PID) {
				ClearStaleLock(pidPath, info.PID)
				return nil
			}
			return serr
		}
		// Poll for exit (daemon Stop drains sessions, checkpoints WAL, closes DB).
		if !waitForExitFn(info.PID, 5*time.Second) {
			// SIGTERM grace expired: the daemon is stuck (e.g. Stop wedged in
			// d.wg.Wait() behind a session that never returns). Before
			// escalating to SIGKILL, re-verify the target is STILL the daemon
			// we recorded: during the grace window the daemon may have exited
			// and its pid been recycled by an unrelated process. Never SIGKILL
			// a process whose identity cannot be confirmed. On mismatch: if
			// the pid is dead, the daemon did exit (just not within the grace
			// window) — clear the stale pidfile and succeed; if the pid is
			// alive but no longer ours, leave the lock untouched and return
			// the refusal error (never signal an innocent process).
			if err := verifyDaemonIdentity(info); err != nil {
				if !IsProcessAlive(info.PID) {
					ClearStaleLock(pidPath, info.PID)
					return nil
				}
				return err
			}
			log.Printf("stale daemon pid=%d did not exit within 5s of SIGTERM; sending SIGKILL", info.PID)
			if serr := syscall.Kill(info.PID, syscall.SIGKILL); serr != nil {
				// Died between the probe and the signal.
				if !IsProcessAlive(info.PID) {
					ClearStaleLock(pidPath, info.PID)
					return nil
				}
				return serr
			}
			if !waitForExitFn(info.PID, 2*time.Second) {
				// Still alive after SIGKILL: never report success. The lock
				// stays (ClearStaleLock below refuses to remove a pidfile
				// naming a live process); the caller gets an explicit error
				// instead of a fake nil that would lead to a spawn attempt
				// doomed to fail on the still-held lock.
				return fmt.Errorf("stale daemon pid=%d still alive after SIGTERM+SIGKILL; lock not cleared", info.PID)
			}
		}
	}
	ClearStaleLock(pidPath, info.PID)
	return nil
}

// waitForExit polls IsProcessAlive(pid) until it reports dead or the timeout
// elapses. Returns true when the process exited within the window.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if !IsProcessAlive(pid) {
			return true
		}
	}
	return false
}

// procStartTimeFn/procCmdlineFn/procEnvironFn/procExeFn are the /proc readers
// used by verifyDaemonIdentity and scanProcs; osExecutableFn resolves the path
// of the running binary. They are variables so tests can simulate an unreadable
// /proc or a different self binary; production behavior always uses the real
// implementations.
var (
	procStartTimeFn = procStartTime
	procCmdlineFn   = procCmdline
	procEnvironFn   = procEnviron
	procExeFn       = procExe
	osExecutableFn  = os.Executable
)

// verifyDaemonIdentity guards the SIGTERM/SIGKILL in KillStaleDaemon against
// two attack shapes:
//
//   - PID reuse (S3): between reading the pidfile and signaling, the recorded
//     pid may have died and been recycled by an unrelated process. The pidfile
//     records the /proc start time of the daemon it was written for
//     (ProcStart); a live process whose /proc start time differs is a
//     different incarnation and must never be signaled.
//
//   - Pidfile forgery (audit): the pidfile itself is UNTRUSTED — any process
//     running as the same user can write a daemon.pid naming an arbitrary
//     victim pid together with the victim's real /proc start time (both are
//     world-readable) and delete the socket to manufacture a stale-daemon
//     scenario. A matching start time therefore only proves that the pidfile
//     points at the same incarnation it points at, which says nothing about
//     the target being a daemon. Identity is additionally bound to facts read
//     from the TARGET process, outside the pidfile — the kill requires at
//     least one of:
//
//     1. /proc/<pid>/exe resolves to this binary (the daemon is always the
//     same binary as the client: SpawnDetached spawns os.Executable());
//     2. /proc/<pid>/environ carries CODEGRAPH_DAEMON_INTERNAL=1 (set by
//     SpawnDetached on every daemon it spawns; unlike the exe path it
//     survives a binary replacement on upgrade, where an old daemon's
//     exe no longer resolves to the current build).
//
//     Both facts live in the target's own /proc entries and cannot be forged
//     into an unrelated process by writing a pidfile.
//
// Fail closed: a pidfile without a recorded start time (legacy format) cannot
// be checked for PID reuse at all, so it never enters the kill path — the
// caller surfaces the refusal and the pidfile is removed manually or via the
// normal ClearStaleLock path once the recorded process is dead. When identity
// cannot be verified — exe unreadable (permission, vanished process, zombie),
// environ unreadable, or neither fact matches — the kill is refused. Returns
// nil only when the target process is positively identified as a codegraph
// daemon.
func verifyDaemonIdentity(info *LockInfo) error {
	if info.ProcStart <= 0 {
		// Legacy pidfile (written before start-time records existed): the
		// PID-reuse guard cannot work, so the kill path is unavailable —
		// fail closed instead of degrading to the weaker cmdline check
		// (argv[0] is trivially spoofed with `exec -a`, and an empty
		// cmdline used to pass as "keep historical behavior").
		return fmt.Errorf("%w: pid %d: pidfile has no recorded process start time (legacy format); cannot verify identity", ErrStaleDaemonRefused, info.PID)
	}
	cur := procStartTimeFn(info.PID)
	if cur == 0 {
		// The pidfile records a start time but /proc no longer reports
		// one: data is insufficient to confirm identity, so refuse rather
		// than degrade to a weaker check (never risk SIGTERMing a process
		// we cannot identify).
		return fmt.Errorf("%w: pid %d: cannot read process start time", ErrStaleDaemonRefused, info.PID)
	}
	if cur != info.ProcStart {
		return fmt.Errorf("%w: pid %d: process start time changed", ErrStaleDaemonRefused, info.PID)
	}
	// Same incarnation the pidfile was written for. Now bind identity to the
	// target process itself, not to pidfile content.
	if exe, err := procExeFn(info.PID); err == nil && exe != "" && isSelfBinary(exe) {
		return nil
	}
	if hasDaemonEnvMarker(procEnvironFn(info.PID)) {
		return nil
	}
	return fmt.Errorf("%w: pid %d: not a codegraph daemon (exe and environ identity checks failed)", ErrStaleDaemonRefused, info.PID)
}

// isSelfBinary reports whether exe is the resolved path of the running binary.
// A real daemon is spawned by SpawnDetached from os.Executable(), so its
// /proc/<pid>/exe always resolves to the same file as the client's.
func isSelfBinary(exe string) bool {
	self, err := osExecutableFn()
	if err != nil || self == "" {
		return false
	}
	return filepath.Clean(exe) == filepath.Clean(self)
}

// hasDaemonEnvMarker reports whether the NUL-joined environ read from
// /proc/<pid>/environ carries CODEGRAPH_DAEMON_INTERNAL=1 — the marker
// SpawnDetached sets on every daemon it spawns (same predicate as
// isInvisibleHolder in recovery.go). An empty environ (unreadable, or a
// zombie's) is a no-match.
func hasDaemonEnvMarker(environ string) bool {
	if environ == "" {
		return false
	}
	for _, kv := range strings.Split(environ, "\x00") {
		if kv == EnvDaemonInternal+"=1" {
			return true
		}
	}
	return false
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

// procExe returns the resolved executable path of pid (readlink of
// /proc/<pid>/exe). It fails on non-procfs platforms, on permission errors,
// and for zombies or vanished processes — callers must treat every failure
// as "identity unverified" (fail closed).
func procExe(pid int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
}

// IsProcessAlive probes pid with kill(2) signal 0 — zero fd allocation;
// avoids Go 1.24+ os.FindProcess, which opens a pidfd per call on Linux and
// never releases it here, so the PPID watchdog (one probe per tick, default
// 5s) leaked one fd per tick per process (unbounded). Semantics: nil →
// alive; ESRCH → dead; EPERM → alive (a process we may not signal, e.g. not
// ours); any other error is treated conservatively as alive so a lock is
// never stolen on uncertainty.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return true
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

package daemon

import (
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

// ErrInvisibleHolderSurvived is wrapped by every invisible-holder cleanup
// error in which a holder is still ALIVE after SIGTERM+SIGKILL. Meaning: the
// flock is still owned by a process that cannot be killed (e.g. stuck in an
// uninterruptible D state). Neither spawning a replacement daemon nor
// falling back to direct mode is safe while it lives (double-write risk), so
// callers must surface it as a fatal error (main exits non-zero with an
// actionable message) instead of retrying. It is distinct from
// ErrStaleDaemonRefused: there the identity could not be confirmed (do not
// signal); here the identity WAS confirmed and the kill simply did not work.
var ErrInvisibleHolderSurvived = errors.New("invisible flock holder survived SIGTERM+SIGKILL")

// waitForExitFn polls IsProcessAlive(pid) until dead or timeout (see
// waitForExit in lock.go). Injectable so tests can simulate a process that
// survives SIGKILL without waiting out the real 5s+2s grace windows.
var waitForExitFn = waitForExit

// procSnapshot is everything killInvisibleHolders needs to know about one
// process: its NUL-joined argv and environ as read from /proc.
type procSnapshot struct {
	pid     int
	cmdline string
	environ string
}

// procEnviron returns the NUL-joined environment of pid from /proc, or ""
// when unavailable (non-procfs platforms, permission, vanished process).
func procEnviron(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	return string(raw)
}

// scanProcsFn lists all processes with their /proc cmdline and environ.
// Injectable so tests can simulate /proc contents (or its absence) without a
// procfs; production always uses the real scan.
var scanProcsFn = scanProcs

// scanProcs enumerates /proc/<pid>/cmdline and /proc/<pid>/environ for every
// process on the host. Unreadable or vanished processes are reported with
// empty strings; the predicate below treats missing data as a no-match
// (conservative: nothing is killed based on data we could not read).
func scanProcs() ([]procSnapshot, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]procSnapshot, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		out = append(out, procSnapshot{
			pid:     pid,
			cmdline: procCmdlineFn(pid),
			environ: procEnvironFn(pid),
		})
	}
	return out, nil
}

// killInvisibleHolders terminates daemon-mode codegraph processes that own
// root's DB flock but are unreachable through every normal discovery
// mechanism ("invisible flock holder").
//
// HOW A HOLDER BECOMES INVISIBLE: deploy.sh used to kill the recorded daemon
// and then unconditionally `rm -f daemon.pid daemon.sock`. A daemon that was
// mid-spawn (or had been re-spawned by a racing client) kept running — and
// kept holding the DB flock — but lost both its pidfile and its socket
// dentry. From then on: clients dial only to find "socket file missing"
// (never a version mismatch), halfDeadDaemonHoldsLock sees no pidfile, and
// every fresh spawn acquires pidfile+socket, then dies in onReady on the
// still-held flock — a permanent failure loop with a self-referential error
// ("held by pid <self>", the pidfile the dying daemon just wrote itself).
//
// SAFETY ARGUMENT: the caller (EnsureAndDial) only enters this branch when
// (a) every candidate socket for root is undialable AND (b) the flock probe
// reports the lock as held. Under that invariant any daemon-mode codegraph
// process targeting root is dead weight: it cannot serve this or any client
// (its socket is gone), it blocks every fresh spawn and every direct-mode
// fallback, and nothing will ever make it exit (its pidfile — the only thing
// that would age it out — was removed). Killing it is the only way out of
// the wedge. The CODEGRAPH_DAEMON_INTERNAL=1 environ check guarantees a
// foreground client or direct-mode process (which may legitimately hold the
// flock while serving a session) is NEVER touched.
//
// A scan failure (e.g. /proc is not enumerable in a container) is NOT fatal:
// it means we cannot identify any holder, so we report zero kills and the
// caller walks its bounded fallback (one more spawn attempt, then direct
// mode) instead of surfacing an error. Only a holder that survives
// SIGTERM+SIGKILL is fatal — it still owns the flock, so neither a fresh
// spawn nor direct-mode fallback is safe while it lives.
func killInvisibleHolders(root string) (int, error) {
	procs, err := scanProcsFn()
	if err != nil {
		// No /proc visibility: we cannot positively identify a holder. Log
		// and treat as "nothing found" — the caller's n==0 path respawns
		// once (bounded) and falls back; never hard-error on a missing or
		// unreadable procfs.
		log.Printf("invisible-holder scan failed (treating as no holders): %v", err)
		return 0, nil
	}
	return killInvisibleHolderProcs(root, procs)
}

// newbornDaemonInStartupGrace reports whether root's pidfile names a LIVE
// process whose pidfile was written less than halfDeadStartupGrace ago. That
// is a daemon spawned moments ago that is still binding its socket (the
// pidfile is written by TryAcquireLock BEFORE the socket bind): it matches
// the invisible-holder profile (pidfile + CODEGRAPH_DAEMON_INTERNAL=1) but
// killing it would SIGTERM a HEALTHY daemon mid-startup — the exact race the
// startup grace in halfDeadDaemonHoldsLock already protects for the
// half-dead branch. The recorded process must be alive: a pidfile naming a
// DEAD process is the failed spawn's own leftover (it died on the still-held
// flock) and must NOT block the cleanup. A missing pidfile is the classic
// wedge (deploy race removed it) — also no grace. Legacy pidfiles without
// startedAt cannot be age-gated: no grace (same conservative choice as
// halfDeadDaemonHoldsLock, which refuses to classify them as half-dead).
func newbornDaemonInStartupGrace(root string) bool {
	raw, err := os.ReadFile(PidPath(root))
	if err != nil {
		return false
	}
	info := DecodeLock(raw)
	if info == nil || info.PID <= 0 || info.StartedAt <= 0 {
		return false
	}
	if !IsProcessAlive(info.PID) {
		return false
	}
	return time.Since(time.UnixMilli(info.StartedAt)) < halfDeadStartupGrace
}

// killInvisibleHolderProcs applies the invisible-holder predicate to a
// process snapshot and terminates every match, stopping at the first
// failure (a holder that survives SIGTERM+SIGKILL keeps the flock —
// proceeding would only spawn another doomed daemon). Returns the number of
// holders killed. Split from killInvisibleHolders so tests can feed
// synthetic /proc data without a procfs.
//
// SNAPSHOT-DECISION + PRE-SIGNAL RECHECK WINDOW: the match above is decided
// on the scan snapshot, which is already stale by the time we signal — the
// process may have exited and its pid been recycled by an unrelated process
// in between (the classic kill(2) TOCTOU). There is no pidfile here, so
// verifyDaemonIdentity's ProcStart comparison cannot be used; instead every
// signal (SIGTERM and the SIGKILL escalation) is preceded by a live re-read
// of /proc/<pid>/cmdline + environ and a re-run of the invisible-holder
// predicate (liveInvisibleHolderMatch). A pid that no longer matches is
// skipped — not signaled, not counted as killed — and the remaining
// candidates are still processed.
func killInvisibleHolderProcs(root string, procs []procSnapshot) (int, error) {
	// Newborn-daemon grace (m3): a pidfile written less than
	// halfDeadStartupGrace ago names a daemon that was just spawned and is
	// still in onReady — it also carries pidfile + CODEGRAPH_DAEMON_INTERNAL,
	// so a two-client race could otherwise kill it while it is healthy.
	// Skip the whole kill pass for root: the newborn gets time to finish
	// binding (then it is dialable and the caller's dial loop connects) or to
	// exit on its own (flock failure releases everything). The wedge scenario
	// (pidfile missing) is unaffected.
	newborn := newbornDaemonInStartupGrace(root)
	if newborn {
		log.Printf("invisible-holder cleanup for %s: pidfile names a newborn daemon in startup grace (< %s); skipping kill pass", root, halfDeadStartupGrace)
	}
	killed := 0
	for _, p := range procs {
		if p.pid == os.Getpid() {
			// Never kill ourselves: this client is not the daemon blocking
			// the spawn, and SIGTERMing ourselves would abort the session.
			continue
		}
		if !isInvisibleHolder(root, p) {
			continue
		}
		if newborn {
			log.Printf("invisible holder candidate pid %d (workdir %s) skipped: newborn daemon in startup grace", p.pid, root)
			continue
		}
		log.Printf("invisible flock holder: pid %d (workdir %s) — terminating", p.pid, root)
		ok, err := terminateInvisibleHolder(root, p.pid)
		if err != nil {
			return killed, err
		}
		if ok {
			killed++
		}
	}
	return killed, nil
}

// isInvisibleHolder reports whether the process snapshot matches the
// invisible-holder profile for root:
//   - argv[0] names a codegraph binary (exact basename/segment match, see
//     isCodegraphCmdline — never a bare substring)
//   - argv contains -workdir with the value root (path-equivalent match:
//     both the daemon and the caller receive the same canonicalized root,
//     so this is exact in practice; Clean only tolerates a trailing slash)
//   - environ carries CODEGRAPH_DAEMON_INTERNAL=1 (daemon mode only)
//
// Empty cmdline or environ (unreadable /proc data) is a no-match —
// conservative: never kill a process we cannot positively identify.
func isInvisibleHolder(root string, p procSnapshot) bool {
	if p.cmdline == "" || p.environ == "" {
		return false
	}
	if !isCodegraphCmdline(p.cmdline) {
		return false
	}
	args := strings.Split(p.cmdline, "\x00")
	workdir := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-workdir" {
			workdir = args[i+1]
			break
		}
	}
	if workdir == "" || filepath.Clean(workdir) != filepath.Clean(root) {
		return false
	}
	for _, kv := range strings.Split(p.environ, "\x00") {
		if kv == EnvDaemonInternal+"=1" {
			return true
		}
	}
	return false
}

// liveInvisibleHolderMatch re-verifies, immediately before a signal is
// sent, that pid is STILL the invisible flock holder for root: it re-reads
// /proc/<pid>/cmdline and /proc/<pid>/environ and re-runs the
// four-condition predicate (codegraph argv0 + -workdir root +
// CODEGRAPH_DAEMON_INTERNAL=1 + not self). A live process's cmdline and
// environ never change, so a mismatch here can only mean the original
// holder exited and the pid was recycled — signaling would hit an innocent
// process. Dead pids read as empty /proc data and are a no-match (same
// conservative rule as isInvisibleHolder).
func liveInvisibleHolderMatch(root string, pid int) bool {
	if pid == os.Getpid() {
		return false
	}
	return isInvisibleHolder(root, procSnapshot{
		pid:     pid,
		cmdline: procCmdlineFn(pid),
		environ: procEnvironFn(pid),
	})
}

// terminateInvisibleHolder SIGTERMs pid, waits up to 5s for exit, then
// escalates to SIGKILL with a 2s wait. Identity is re-verified against live
// /proc immediately before EVERY signal — closing the snapshot→signal
// window described on killInvisibleHolderProcs, and the SIGTERM-grace →
// SIGKILL window as well (the holder may have exited in the grace period
// and its pid been recycled).
//
// Primary path: terminateViaPidfd pins the process with a pidfd so the
// recheck→signal window is closed even across pid reuse (see pidfd.go).
// When the platform/kernel cannot provide a pidfd (ErrPidfdNotSupported),
// it falls back to terminateViaKill, which keeps the historical
// kill(2) + live-recheck semantics (the rechecks carry the identity
// guarantee there; only the last microseconds before each signal remain
// unclosable). On recheck mismatch the pid is skipped: (false, nil),
// never counted as killed, no error. Returns (true, nil) when the process
// exited; an error only when the process survives even SIGKILL — wrapped
// with ErrInvisibleHolderSurvived (the flock is still held — the caller
// must not proceed).
func terminateInvisibleHolder(root string, pid int) (bool, error) {
	if !liveInvisibleHolderMatch(root, pid) {
		log.Printf("pid %d no longer matches the invisible-holder profile (died/recycled since scan); skipping", pid)
		return false, nil
	}
	ok, err := terminateViaPidfd(root, pid)
	if err != nil && errors.Is(err, ErrPidfdNotSupported) {
		// pidfd_open unavailable or failed (non-Linux, ENOSYS, EPERM, ...):
		// graceful fallback to the classic path — the rechecks still make it
		// safe, only the pid-reuse-free guarantee is lost. kill(2) allocates
		// no pidfd, so the fallback leaks nothing either.
		log.Printf("pidfd path unavailable for pid %d (%v); falling back to kill(2)+recheck", pid, err)
		return terminateViaKill(root, pid)
	}
	return ok, err
}

// terminateViaKill is the classic fallback: SIGTERM → 5s wait → SIGKILL →
// 2s wait via syscall.Kill, with a live /proc identity recheck before EVERY
// signal (PID-reuse guard). Unlike os.FindProcess + Signal, kill(2) allocates
// no pidfd, so this path leaks nothing on Go 1.24+ Linux either. Kept for
// platforms and kernels without pidfd support; see terminateInvisibleHolder.
func terminateViaKill(root string, pid int) (bool, error) {
	if !liveInvisibleHolderMatch(root, pid) {
		log.Printf("pid %d no longer matches the invisible-holder profile (died/recycled since scan); skipping", pid)
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if !IsProcessAlive(pid) {
			return false, nil // died between recheck and signal
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
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if !IsProcessAlive(pid) {
			return false, nil
		}
		return false, err
	}
	if !waitForExitFn(pid, 2*time.Second) {
		return false, fmt.Errorf("%w: pid %d still alive after SIGTERM+SIGKILL; flock not released", ErrInvisibleHolderSurvived, pid)
	}
	return true, nil
}

// invisibleHolderHoldsLock probes whether root's DB flock is held — the
// trigger condition for the invisible-holder cleanup. This is a LIGHTWEIGHT
// flock(2) probe, NOT a full db.Open: a db.Open would create the .codegraph
// directory, run schema/FTS backfill work, and — when the flock is actually
// free — briefly take the write lock itself, which both slows the common
// path and can make a concurrent spawn fail once transiently. (db.Open has
// the same flock logic in its own package; it is duplicated here only to
// avoid an import cycle.)
//
// Semantics: LOCK_EX|LOCK_NB failing with EWOULDBLOCK/EAGAIN means another
// process holds the lock → true (held). A successful acquisition is
// released immediately and means free → false. An open failure (e.g. the
// .codegraph directory or the lock file does not exist — no daemon could
// hold a lock that does not exist) is free, not held; any other flock error
// is also free: conservative in the kill direction — never kill based on a
// probe we cannot trust. The probe opens WITHOUT O_CREATE on purpose: a
// read-only probe must not leave an empty lock file behind in a directory
// where the daemon has not run yet (a lightweight side effect the audit
// flagged).
func invisibleHolderHoldsLock(root string) bool {
	lockPath := filepath.Join(CodeGraphDir(root), "codegraph.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		return false // lock file or its directory missing → nothing holds it
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true
		}
		return false
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

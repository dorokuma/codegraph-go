package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dorokuma/codegraph-go/internal/cgdir"
	"golang.org/x/sys/unix"
)

// SpawnOpts carries parent CLI flags the detached daemon should inherit.
type SpawnOpts struct {
	ConfigFile string // -config path (optional)
	NoSync     bool   // -no-sync
	// CPUQuotaPercent, when > 0, asks SpawnDetached to wrap the daemon in a
	// systemd-run transient scope with CPUQuota=<N>% (150 = 1.5 cores).
	// Falls back to a plain spawn when systemd is unavailable or the scope
	// launch fails — never blocks daemon start.
	CPUQuotaPercent int
}

// spawnDetachedFn is the spawner used by EnsureAndDial. A variable so tests
// can substitute a fake daemon starter (a real spawn would re-execute the
// test binary); production always uses SpawnDetached.
var spawnDetachedFn = SpawnDetached

// Indirection for tests: resolveDaemonBinary and systemdRunArgs as
// variables let spawn tests fake the daemon binary and force the
// systemd-run wrapper path without a real systemd host. Production keeps
// the real implementations.
var (
	resolveDaemonBinaryFn = resolveDaemonBinary
	systemdRunArgsFn      = systemdRunArgs
)

// SpawnDetached launches this binary as the shared daemon for root.
// Stdio goes to .codegraph/daemon.log; the child is in its own session.
// Fails with a phase/root/path/errno error when the log cannot be opened
// instead of spawning a child whose output would be discarded. opts may be
// nil.
// resolveDaemonBinary returns the path of this binary to spawn as the
// detached daemon. It prefers os.Executable() — the real, resolved path of
// the running client — so the daemon is always the same binary (same
// version) as the client.
//
// The only exception is `go run` (and `go test`): there os.Executable()
// points into a temporary build directory (/tmp/go-buildNNN/... on Linux,
// the TMPDIR equivalent on macOS) that is removed when the run exits. A
// daemon spawned from such a path would re-execute a dead file, so in that
// dev scenario alone we fall back to the path the process was invoked with
// (os.Args[0]).
//
// We must NOT unconditionally trust argv[0]. A workspace often contains a
// stale build artifact named after the binary (e.g. ./codegraph-go left in
// the repo root). When the client is started with a relative argv[0] from
// such a directory, filepath.Abs(argv[0]) resolves to that stale artifact;
// spawning it as the daemon splits daemon version from client version, the
// MCP handshake version check fails forever, and the kill-stale-respawn
// loop re-executes the same stale binary on every attempt.
func resolveDaemonBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	argv0 := ""
	if len(os.Args) > 0 {
		argv0 = os.Args[0]
	}
	return resolveDaemonBinaryFrom(self, argv0), nil
}

// resolveDaemonBinaryFrom implements the binary selection policy given the
// resolved executable path and the argv[0] the process was invoked with.
// Split out so tests can exercise the policy without re-executing a binary.
func resolveDaemonBinaryFrom(executable, argv0 string) string {
	self := executable
	if isGoRunTempPath(self) && argv0 != "" {
		if abs, aerr := filepath.Abs(argv0); aerr == nil {
			if st, serr := os.Stat(abs); serr == nil && !st.IsDir() {
				self = abs
			}
		}
	}
	return self
}

// isGoRunTempPath reports whether p is the temporary build path `go run`
// (or `go test`) uses for the binary, e.g.
// /tmp/go-build1234567890/b001/exe/codegraph-go on Linux or the equivalent
// under TMPDIR (like /var/folders/.../T/go-build...) on macOS. Such paths
// are deleted when the run exits and must never be used to spawn a daemon.
func isGoRunTempPath(p string) bool {
	dir := filepath.Dir(p)
	// go run places the binary at <tmp>/go-build<NNN>/<id>/exe/<name>.
	return strings.HasSuffix(dir, string(filepath.Separator)+"exe") &&
		strings.Contains(dir, string(filepath.Separator)+"go-build")
}

// spawnEarlyExitWindow bounds how long SpawnDetached watches a systemd-run
// scope launch for an immediate non-zero exit before treating it as a
// healthy startup.
const spawnEarlyExitWindow = 150 * time.Millisecond

// daemonLogTailBytes bounds the daemon.log tail embedded in early-exit
// diagnostics — enough for the daemon's final fatal lines, small enough to
// stay a single warn record.
const daemonLogTailBytes = 4 * 1024

func SpawnDetached(root string, opts *SpawnOpts) error {
	// The .codegraph guard runs before anything under the project is
	// touched: daemon.log below — and everything the daemon writes later —
	// must land in the real .codegraph, never through a symlink at its
	// target. On failure nothing under the project is written.
	if _, err := cgdir.Ensure(CodeGraphDir(root)); err != nil {
		return err
	}
	self, err := resolveDaemonBinaryFn()
	if err != nil {
		return err
	}
	logPath := filepath.Join(CodeGraphDir(root), "daemon.log")
	// Rotate if the log has grown past 10 MB — keep one previous copy.
	// Lstat (never Stat): a symlinked or otherwise non-regular leaf must not
	// enter rotation — following a link would stat and rename the TARGET,
	// and a rotated symlink leaf would still redirect the open below to a
	// file outside the project. Non-regular leafs are left in place and
	// rejected at open time instead.
	const maxLogSize = 10 * 1024 * 1024
	if fi, statErr := os.Lstat(logPath); statErr == nil && fi.Mode().IsRegular() && fi.Size() > maxLogSize {
		_ = os.Rename(logPath, logPath+".1")
	}
	// Fail instead of spawning blind: when daemon.log cannot be opened the
	// child would run with stdout/stderr discarded, and the parent would
	// learn nothing about why the daemon died (a read-only .codegraph was
	// the original incident — EACCES here left zero evidence). Returning
	// the phase + target root + path + errno lets EnsureAndDial surface a
	// real error while still falling back to direct mode safely.
	// O_RDWR (not O_WRONLY): the same open file description is handed to the
	// exit watcher for the bounded diagnostic tail — the tail is read through
	// THIS fd, never by re-opening logPath, so a rotation, a rename, or a
	// swapped symlink at that path cannot redirect the read to a different
	// file. The open itself must not follow a leaf symlink either: a planted
	// link used to redirect the child's stdout/stderr — and this fd's tail —
	// to an arbitrary file outside the project, so openDaemonLog pins the
	// leaf to a regular file at the syscall boundary (see below). Ownership
	// of logFile transfers to whichever goroutine owns the child wait below;
	// every error path closes it explicitly.
	logFile, err := openDaemonLog(logPath)
	if err != nil {
		return fmt.Errorf("spawn daemon for %s: open daemon log %s: %w", root, logPath, err)
	}

	args := []string{"-workdir", root}
	if opts != nil {
		if opts.ConfigFile != "" {
			args = append(args, "-config", opts.ConfigFile)
		}
		if opts.NoSync {
			args = append(args, "-no-sync")
		}
	}
	env := os.Environ()
	// Mark as daemon; scrub any stale host-ppid markers.
	filtered := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, EnvDaemonInternal+"=") {
			continue
		}
		filtered = append(filtered, e)
	}
	filtered = append(filtered, EnvDaemonInternal+"=1")
	// daemonCmd builds a detached child with identical env/dir/session/stdio
	// for the primary spawn and every systemd-run fallback respawn below.
	daemonCmd := func(name string, cargs ...string) *exec.Cmd {
		c := exec.Command(name, cargs...)
		c.Env = filtered
		c.Dir = root
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		c.Stdout = logFile
		c.Stderr = logFile
		c.Stdin = nil
		return c
	}
	cmd := daemonCmd(self, args...)
	quotaArgs := systemdRunArgsFn(self, args, opts)
	if quotaArgs != nil {
		cmd = daemonCmd(quotaArgs[0], quotaArgs[1:]...)
	}

	if err := cmd.Start(); err != nil {
		if quotaArgs != nil {
			// Zero-fault rollback: a transient scope that failed to launch
			// (permissions, cgroup v1-only host, transient unit policy) must
			// not prevent the daemon from starting — respawn directly.
			slog.Warn("systemd-run spawn failed, falling back to direct spawn", "error", err)
			// The wrapper is gone: clear it BEFORE the reaper below, or the
			// direct child would still be treated as a scoped spawn and run
			// into the async probe window — where a normal lock-race loser
			// exiting 0 looks like a scope-start failure and spawns the
			// daemon a second time.
			quotaArgs = nil
			cmd = daemonCmd(self, args...)
			if err = cmd.Start(); err != nil {
				_ = logFile.Close()
				return fmt.Errorf("spawn daemon for %s: %w", root, err)
			}
		} else {
			_ = logFile.Close()
			return fmt.Errorf("spawn daemon for %s: %w", root, err)
		}
	}
	// The child is a Setsid session leader and is adopted by init if this
	// process dies, so reaping does not affect daemon independence
	// (cmd.Wait() and Setsid do not conflict — Wait is a plain waitpid on
	// the child).
	if quotaArgs != nil {
		// The buffered channel also doubles as the async-failure probe for
		// the systemd-run path: when the wrapper was used, we watch a short
		// window for an immediate non-zero exit (scope creation failed —
		// Start() succeeds because systemd-run itself launched fine) and fall
		// back to a direct spawn, same zero-fault contract as the sync path.
		waitCh := make(chan error, 1)
		go func() {
			waitCh <- cmd.Wait()
		}()
		select {
		case waitErr := <-waitCh:
			if waitErr != nil && !daemonAliveAt(root) {
				// The wrapper exited non-zero before any daemon took the lock:
				// systemd-run failed to create the scope asynchronously (e.g.
				// transient-unit policy refused at scope start, not at exec).
				// A normal lock-race loser exits 0, and a winner keeps the pid
				// file owned — neither must trigger the fallback.
				slog.Warn("systemd-run scope start failed, falling back to direct spawn", "error", waitErr)
				cmd = daemonCmd(self, args...)
				if err := cmd.Start(); err != nil {
					_ = logFile.Close()
					return fmt.Errorf("spawn daemon for %s: %w", root, err)
				}
				go watchDaemonExit(cmd, root, logFile, logPath)
			} else {
				// Wrapper exited cleanly, or a live daemon already holds the
				// lock: no fallback, nobody else needs the fd.
				_ = logFile.Close()
			}
		case <-time.After(spawnEarlyExitWindow):
			// Still running after the window: normal startup (the daemon or
			// the systemd-run wrapper supervising it). Keep watching the
			// reaper channel so a scope that fails asynchronously — e.g.
			// D-Bus congestion delaying the transient-unit failure past the
			// window — leaves a warn trail instead of a silent exit. The
			// client still falls back to direct mode via the dial timeout;
			// this is observability only.
			go func() {
				defer logFile.Close()
				if waitErr := <-waitCh; waitErr != nil {
					slog.Warn("systemd-run scope exited with error after startup window", "error", waitErr)
				}
			}()
		}
	} else {
		// Plain spawn: same reaping duty, plus the diagnostic trail the
		// client otherwise never gets — a daemon that dies non-zero while no
		// live daemon holds the lock used to leave its Wait result dropped
		// in the channel, and the parent only saw the dial time out.
		go watchDaemonExit(cmd, root, logFile, logPath)
	}
	return nil
}

// openDaemonLog opens daemon.log for the child's stdout/stderr under a leaf
// safety contract the plain os.OpenFile(path, O_RDWR|O_CREAT) call could
// not give: the kernel must refuse the open when the leaf itself is a
// symlink — a planted link is never resolved into its target.
//
//   - The open is a single unix.Open(logPath, O_RDWR|O_APPEND|O_CREAT|
//     O_NOFOLLOW|O_CLOEXEC, 0600): O_NOFOLLOW makes the kernel refuse a
//     symlinked FINAL component at open() time with ELOOP, before
//     O_CREAT/O_APPEND ever touch a target — a leaf link planted at
//     daemon.log cannot redirect the child's output (and the parent's
//     diagnostic tail) to an arbitrary file, and a dangling link does not
//     get its target created. O_NOFOLLOW deliberately constrains only the
//     leaf: intermediate components keep following symlinks because a
//     symlinked workdir ancestor is a supported alias (cgdir.Ensure
//     verifies only that .codegraph itself resolves to a real directory),
//     so openat2 RESOLVE_NO_SYMLINKS / RESOLVE_BENEATH-style re-rooting
//     would reject supported layouts and is out of scope here. The
//     residual boundary: an ancestor or .codegraph swap racing between
//     Ensure and this open is not bound by an fd — the leaf guarantee
//     does not extend to parent re-rooting (unchanged task boundary).
//   - After open, f.Stat must report a regular file on the bound fd:
//     FIFOs, devices and anything else the kernel let past O_NOFOLLOW are
//     refused there. A leaf swapped or unlinked after open is fine — the
//     fd is already bound to the inode the kernel opened, same standing
//     as a later rotation, so no post-open Lstat/SameFile re-check is
//     needed (and a gone leaf is NOT treated as suspicious).
//
// Refusals keep errno identity: a leaf directory surfaces EISDIR through
// the open, leaf links surface ELOOP as the kernel's own errno (no
// normalization re-check needed), and every failure closes its own fd
// before returning so the caller never spawns a blind child.
func openDaemonLog(logPath string) (*os.File, error) {
	fd, err := unix.Open(logPath,
		unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), logPath)
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("daemon log leaf is not a regular file: %s", logPath)
	}
	return f, nil
}

// watchDaemonExit reaps a spawned daemon child and, when it exits with an
// error while no live daemon is recorded in root's pidfile, leaves the
// diagnostic trail the client otherwise never sees. It reports only what it
// actually observed: the wait error (exit status or signal), the target
// root, the log path, and — when the log behind logFile is a regular file —
// a bounded scrubbed tail. A lock-race loser exits 0 and a surviving winner
// keeps the pidfile owned; both take the silent return. daemonAliveAt reads
// the pidfile only — a live daemon holding the flock WITHOUT a pidfile (the
// invisible-holder case handled in proxy.go) is invisible to this probe, so
// the warn message claims nothing about flock holders. Callers run it in a
// goroutine; Wait blocks until the child exits, which for a healthy daemon
// is the daemon's whole lifetime (same lifetime bound the bare reaper
// goroutine had before), and logFile stays open for that span so the tail
// reads the same object the child wrote.
func watchDaemonExit(cmd *exec.Cmd, root string, logFile *os.File, logPath string) {
	defer logFile.Close()
	waitErr := cmd.Wait()
	if waitErr == nil || daemonAliveAt(root) {
		return
	}
	attrs := []any{"error", waitErr, "root", root, "log", logPath}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ProcessState.Exited() {
		attrs = append(attrs, "exit_code", exitErr.ExitCode())
	}
	if tail := daemonLogTail(logFile); tail != "" {
		attrs = append(attrs, "log_tail", tail)
	}
	slog.Warn("spawned daemon exited; no live daemon recorded in pidfile", attrs...)
}

// daemonLogTail returns a bounded, scrubbed tail of the daemon log behind f
// — the same open file description the spawned child wrote to, never a
// re-open of the log path, so rotation, renames, or symlink swaps at that
// path cannot redirect the read elsewhere. Only regular files are read: a
// pre-planted FIFO or device yields no tail at all instead of a blocking
// read. f's O_APPEND position is irrelevant; ReadAt does not move it.
//
// Redaction boundary: the raw window is the file's last daemonLogTailBytes.
// Truncating mid-line can split a secret across the window edge (the
// "token=" prefix just before the cut, the value inside) — no pattern can
// re-identify such a fragment — so the leading partial line is dropped
// wholesale and only complete lines are emitted. Scrubbing runs on that
// complete-line body, then the SAME complete-line rule clamps the result
// back to daemonLogTailBytes: a byte-level cut could land inside a record
// and strip the identifying prefix safelog's own patterns still need (a
// "Basic dXNlcjpwYXNz" reduced to raw base64 is unmaskable downstream), so
// over-limit output drops whole leading records and a single oversized
// record is omitted entirely. The emitted tail is always <=
// daemonLogTailBytes and always a whole number of lines.
func daemonLogTail(f *os.File) string {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return ""
	}
	size := fi.Size()
	off := int64(0)
	truncated := false
	if size > daemonLogTailBytes {
		off = size - daemonLogTailBytes
		truncated = true
	}
	buf := make([]byte, size-off)
	nr, _ := f.ReadAt(buf, off)
	raw := string(buf[:nr])
	if truncated {
		// Drop the leading partial line entirely (see the redaction
		// boundary note above).
		i := strings.IndexByte(raw, '\n')
		if i < 0 {
			return ""
		}
		raw = raw[i+1:]
	}
	out := scrubDiagText(raw)
	// Second trim, same rule as the first: only complete lines are emitted.
	// Scrubbing expands secrets into longer markers ("pwd=x" grows 10 bytes),
	// so the post-scrub body can exceed the bound; a raw byte cut there would
	// drop the identifying prefix of the first surviving record (e.g. leave
	// the tail of "Basic dXNlcjpwYXNz" without the "Basic" the production
	// safelog needs to mask it), handing the sink an unmaskable fragment.
	// Whole records are dropped from the front — the oldest evidence first —
	// until the emission fits; a single record larger than the bound is
	// dropped entirely rather than emitted mid-secret. Cutting at '\n'
	// boundaries also can never split a multi-byte UTF-8 rune.
	for len(out) > daemonLogTailBytes {
		i := strings.IndexByte(out, '\n')
		if i < 0 {
			// No complete record fits the bound: safe omission.
			return ""
		}
		out = out[i+1:]
	}
	return out
}

// diagScrubPatterns is a compact subset of internal/safelog's secret
// shapes, kept local because the daemon package must not depend on the
// logging setup: SetupLogger already scrubs the sinks in production, this
// is the defense-in-depth pass for consumers that render slog records or
// SpawnDetached errors around an unconfigured handler (tests, embedders).
var diagScrubPatterns = []*regexp.Regexp{
	// key=value / key: value pairs with sensitive names.
	regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret|private[_-]?key|authorization|credential|session[_-]?id)\s*[=:]\s*)[^,;\n]+`),
	// Bearer tokens and JWTs.
	regexp.MustCompile(`(?i)(\bbearer\s+)[-a-z0-9._~+/=:_]{8,}`),
	regexp.MustCompile(`\beyJ[a-z0-9_-]{10,}\.[a-z0-9_-]{10,}\.[a-z0-9_-]{10,}\b`),
	// GitHub tokens (classic + fine-grained).
	regexp.MustCompile(`\b(gh[pousr]_|github_pat_)[a-z0-9_]{20,}`),
}

// scrubDiagText masks known secret shapes before diagnostic text (child
// stderr / log tails) is embedded in warnings returned or logged by the
// spawn path.
func scrubDiagText(s string) string {
	for _, re := range diagScrubPatterns {
		s = re.ReplaceAllString(s, "${1}[REDACTED]")
	}
	return s
}

// daemonAliveAt reports whether a live daemon already holds root's lock —
// used to distinguish a systemd-run scope-start failure (nobody holds the
// pidfile yet) from a normal daemon that just won the race.
//
// The pidfile is a diagnostic READ surface only, but it is still read under
// the leaf-safety contract of readRegularPidfile (shared by every
// diagnostic pidfile probe in this package): O_NOFOLLOW refuses a symlinked
// leaf (ELOOP) so a planted link can never redirect the read to an
// arbitrary file outside the project (a forged LockInfo there could
// otherwise suppress the exit warn by naming a live pid), and O_NONBLOCK
// makes the open itself return immediately on a FIFO or device leaf instead
// of parking the watcher forever in open(2) — a writerless FIFO pidfile
// used to wedge watchDaemonExit after the child was already reaped,
// starving the deferred logFile close forever. Every failure path (open
// errno, non-regular leaf, read error) degrades to "no live daemon
// recorded": this is a best-effort observability probe, never a panic
// source.
func daemonAliveAt(root string) bool {
	raw, ok := readRegularPidfile(PidPath(root))
	if !ok {
		return false
	}
	info := DecodeLock(raw)
	if info == nil || info.PID <= 0 || !IsProcessAlive(info.PID) {
		return false
	}
	// PID-reuse guard, reusing the same procStartTime mechanism as
	// verifyDaemonIdentity: a live pid whose current /proc start time differs
	// from the recorded one is a different incarnation, so the pidfile holder
	// is gone and the lock is free to take. got == 0 (non-/proc platforms or
	// an unreadable stat) keeps the plain alive-probe verdict, and pidfiles
	// written before ProcStart existed (info.ProcStart == 0) stay compatible.
	if info.ProcStart > 0 {
		if got := procStartTime(info.PID); got > 0 && got != info.ProcStart {
			return false
		}
	}
	return true
}

// systemdRunArgs returns the argv for launching the daemon through a
// systemd-run transient CPU quota scope, or nil when the quota feature is
// off or unavailable on this host. Split out so tests can exercise the
// availability gate and argv construction without spawning anything.
//
// Availability gate (all must hold): opts.CPUQuotaPercent > 0, Linux
// (runtime.GOOS), root (euid 0 — a non-root user cannot create transient
// scopes on the system manager and user managers are not assumed), a
// systemd system manager (/run/systemd/system exists) and systemd-run on
// PATH. Falling back to a plain spawn keeps every container / non-systemd
// host working unchanged.
func systemdRunArgs(self string, args []string, opts *SpawnOpts) []string {
	return systemdRunArgsFor(os.Geteuid(), self, args, opts)
}

// systemdRunArgsFor is the euid-parameterized core of systemdRunArgs so
// tests can drive both the root and non-root branches deterministically.
func systemdRunArgsFor(euid int, self string, args []string, opts *SpawnOpts) []string {
	if opts == nil || opts.CPUQuotaPercent <= 0 {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	if euid != 0 {
		return nil
	}
	if fi, err := os.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		return nil
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil
	}
	quota := fmt.Sprintf("CPUQuota=%d%%", opts.CPUQuotaPercent)
	// --no-ask-password: never let the wrapper block on an interactive
	// authorization prompt — the daemon spawn path must stay fully
	// non-interactive.
	argv := []string{systemdRun, "--quiet", "--no-ask-password", "--scope", "-p", quota, "--", self}
	return append(argv, args...)
}

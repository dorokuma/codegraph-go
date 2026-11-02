package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dorokuma/codegraph-go/internal/cgdir"
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
// opts may be nil.
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
	const maxLogSize = 10 * 1024 * 1024
	if fi, statErr := os.Stat(logPath); statErr == nil && fi.Size() > maxLogSize {
		_ = os.Rename(logPath, logPath+".1")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		logFile = nil
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
	cmd := exec.Command(self, args...)
	quotaArgs := systemdRunArgsFn(self, args, opts)
	if quotaArgs != nil {
		cmd = exec.Command(quotaArgs[0], quotaArgs[1:]...)
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
	cmd.Env = filtered
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	} else {
		cmd.Stdout = nil
		cmd.Stderr = nil
	}
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		if quotaArgs != nil {
			// Zero-fault rollback: a transient scope that failed to launch
			// (permissions, cgroup v1-only host, transient unit policy) must
			// not prevent the daemon from starting — respawn directly.
			slog.Warn("systemd-run spawn failed, falling back to direct spawn", "error", err)
			// The wrapper is gone: clear it BEFORE the reaper below, or the
			// direct child would still be treated as a scoped spawn and run
			// into the 150ms async probe window — where a normal lock-race
			// loser exiting 0 looks like a scope-start failure and spawns the
			// daemon a second time.
			quotaArgs = nil
			cmd = exec.Command(self, args...)
			cmd.Env = filtered
			cmd.Dir = root
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if logFile != nil {
				cmd.Stdout = logFile
				cmd.Stderr = logFile
			}
			cmd.Stdin = nil
			if err = cmd.Start(); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	// Reap the child in a goroutine instead of Process.Release: a candidate
	// daemon that loses the lock race exits within ~50ms, and without a Wait
	// the parent would carry a zombie until the CLI itself exits. cmd.Wait()
	// reaps it immediately; a daemon that survives long-term just blocks the
	// goroutine on Wait — acceptable for the CLI's short lifetime. The child
	// is a Setsid session leader and is adopted by init if this process dies,
	// so reaping does not affect daemon independence (cmd.Wait() and Setsid
	// do not conflict — Wait is a plain waitpid on the child).
	//
	// The buffered channel also doubles as the async-failure probe for the
	// systemd-run path below: when the wrapper was used, we watch a short
	// window for an immediate non-zero exit (scope creation failed —
	// Start() succeeds because systemd-run itself launched fine) and fall
	// back to a direct spawn, same zero-fault contract as the sync path.
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	if quotaArgs != nil {
		select {
		case waitErr := <-waitCh:
			if waitErr != nil && !daemonAliveAt(root) {
				// The wrapper exited non-zero before any daemon took the lock:
				// systemd-run failed to create the scope asynchronously (e.g.
				// transient-unit policy refused at scope start, not at exec).
				// A normal lock-race loser exits 0, and a winner keeps the pid
				// file owned — neither must trigger the fallback.
				slog.Warn("systemd-run scope start failed, falling back to direct spawn", "error", waitErr)
				cmd = exec.Command(self, args...)
				cmd.Env = filtered
				cmd.Dir = root
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				if logFile != nil {
					cmd.Stdout = logFile
					cmd.Stderr = logFile
				}
				cmd.Stdin = nil
				if err := cmd.Start(); err != nil {
					return err
				}
				go func() {
					_ = cmd.Wait()
				}()
			}
		case <-time.After(150 * time.Millisecond):
			// Still running after the window: normal startup (the daemon or
			// the systemd-run wrapper supervising it). Keep watching the
			// reaper channel so a scope that fails asynchronously — e.g.
			// D-Bus congestion delaying the transient-unit failure past the
			// window — leaves a warn trail instead of a silent exit. The
			// client still falls back to direct mode via the dial timeout;
			// this is observability only.
			go func() {
				if waitErr := <-waitCh; waitErr != nil {
					slog.Warn("systemd-run scope exited with error after startup window", "error", waitErr)
				}
			}()
		}
	}
	return nil
}

// daemonAliveAt reports whether a live daemon already holds root's lock —
// used to distinguish a systemd-run scope-start failure (nobody holds the
// pidfile yet) from a normal daemon that just won the race.
func daemonAliveAt(root string) bool {
	raw, err := os.ReadFile(PidPath(root))
	if err != nil {
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

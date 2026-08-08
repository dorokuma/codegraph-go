package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// SpawnOpts carries parent CLI flags the detached daemon should inherit.
type SpawnOpts struct {
	ConfigFile string // -config path (optional)
	NoSync     bool   // -no-sync
}

// spawnDetachedFn is the spawner used by EnsureAndDial. A variable so tests
// can substitute a fake daemon starter (a real spawn would re-execute the
// test binary); production always uses SpawnDetached.
var spawnDetachedFn = SpawnDetached

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
	self, err := resolveDaemonBinary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(CodeGraphDir(root), 0o700); err != nil {
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
		return err
	}
	// Reap the child in a goroutine instead of Process.Release: a candidate
	// daemon that loses the lock race exits within ~50ms, and without a Wait
	// the parent would carry a zombie until the CLI itself exits. cmd.Wait()
	// reaps it immediately; a daemon that survives long-term just blocks the
	// goroutine on Wait — acceptable for the CLI's short lifetime. The child
	// is a Setsid session leader and is adopted by init if this process dies,
	// so reaping does not affect daemon independence (cmd.Wait() and Setsid
	// do not conflict — Wait is a plain waitpid on the child).
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

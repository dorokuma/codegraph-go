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

// SpawnDetached launches this binary as the shared daemon for root.
// Stdio goes to .codegraph/daemon.log; the child is in its own session.
// opts may be nil.
func SpawnDetached(root string, opts *SpawnOpts) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	// Prefer the path we were invoked with when it's absolute (dev `go run` etc.).
	if len(os.Args) > 0 {
		if abs, aerr := filepath.Abs(os.Args[0]); aerr == nil {
			if st, serr := os.Stat(abs); serr == nil && !st.IsDir() {
				self = abs
			}
		}
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

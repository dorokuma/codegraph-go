package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// SpawnDetached launches this binary as the shared daemon for root.
// Stdio goes to .codegraph/daemon.log; the child is in its own session.
func SpawnDetached(root string) error {
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

	cmd := exec.Command(self, "-workdir", root)
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
	// Detach: don't keep a zombie; child is session leader.
	if err := cmd.Process.Release(); err != nil {
		log.Printf("release daemon process pid=%d: %v", cmd.Process.Pid, err)
	}
	return nil
}

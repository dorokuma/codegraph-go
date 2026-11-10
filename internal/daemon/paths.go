// Package daemon implements the shared MCP daemon (official issue #411 shape):
// one writer process per project root, N thin stdio proxies over a Unix socket.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// PackageVersion is the wire-facing version. Proxy and daemon must match exactly.
const PackageVersion = "0.9.13"

// Soft upper bound for in-project AF_UNIX paths (macOS ~104, Linux ~108).
const posixSocketPathLimit = 100

// LockInfo is the body of .codegraph/daemon.pid.
type LockInfo struct {
	PID        int    `json:"pid"`
	Version    string `json:"version"`
	SocketPath string `json:"socketPath"`
	StartedAt  int64  `json:"startedAt"` // unix ms
	// ProcStart is the /proc/<pid>/stat starttime of the owning process at
	// acquire time (0 on platforms without /proc, or for pidfiles written by
	// older builds). KillStaleDaemon compares it against the live /proc value
	// before signaling to detect PID reuse (S3), and refuses the kill path
	// entirely when it is absent: a pidfile without it cannot prove which
	// process incarnation it names, so a kill must never be based on it.
	ProcStart int64 `json:"procStart,omitempty"`
	// DirDev/DirIno record the (st_dev, st_ino) identity of the .codegraph
	// directory as it was when this pidfile was written (absent for pidfiles
	// from older builds and on platforms without a stat-able directory
	// identity). A same-inode swap of .codegraph (rename + symlink pointing
	// back at the original inode) passes every existing guard with the
	// daemon's integrity intact but used to leave no audit trail; recording
	// the start-time identity here (and in the daemon.log start line) makes
	// such swaps traceable after the fact. Observability only: these fields
	// deliberately do NOT participate in verifyDaemonIdentity or any kill
	// decision, and absent/zero values decode exactly like before.
	DirDev uint64 `json:"dirDev,omitempty"`
	DirIno uint64 `json:"dirIno,omitempty"`
}

// CodeGraphDir returns <root>/.codegraph.
func CodeGraphDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".codegraph")
}

// PidPath is the exclusive lockfile for the project daemon.
func PidPath(projectRoot string) string {
	return filepath.Join(CodeGraphDir(projectRoot), "daemon.pid")
}

// projectHash is a short stable id for tmpdir socket names.
func projectHash(projectRoot string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(projectRoot)))
	return hex.EncodeToString(sum[:])[:16]
}

func tmpdirSocketPath(projectRoot string) string {
	return filepath.Join(os.TempDir(), "codegraph-go-"+projectHash(projectRoot)+".sock")
}

// SocketCandidates returns ordered bind/connect paths, preferred first.
// In-project .codegraph/daemon.sock, then deterministic tmpdir fallback when
// the path is too long or the FS cannot host AF_UNIX.
func SocketCandidates(projectRoot string) []string {
	if runtime.GOOS == "windows" {
		// Named pipes are out of scope for v1; callers fall back to direct mode.
		return nil
	}
	inProject := filepath.Join(CodeGraphDir(projectRoot), "daemon.sock")
	tmp := tmpdirSocketPath(projectRoot)
	if len(inProject) > posixSocketPathLimit {
		return []string{tmp}
	}
	return []string{inProject, tmp}
}

// PreferredSocket is candidate 0 (informational / lockfile default).
func PreferredSocket(projectRoot string) string {
	c := SocketCandidates(projectRoot)
	if len(c) == 0 {
		return ""
	}
	return c[0]
}

// EncodeLock serializes LockInfo as pretty JSON + newline.
func EncodeLock(info LockInfo) []byte {
	b, _ := json.MarshalIndent(info, "", "  ")
	return append(b, '\n')
}

// readRegularPidfile is the shared leaf-safe pidfile READ for the daemon
// package's diagnostic probes (daemonAliveAt, halfDeadDaemonHoldsLock,
// newbornDaemonInStartupGrace): a single
// unix.Open(path, O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC) pins the leaf
// at the syscall boundary — O_NOFOLLOW makes the kernel refuse a symlinked
// final component (ELOOP) so a planted link can never redirect the read to
// an arbitrary file outside the project (a forged LockInfo there could
// otherwise name a live pid and suppress a kill/warn), and O_NONBLOCK
// makes the open itself return immediately on a FIFO or device leaf
// instead of parking the caller forever in open(2). Type and content
// checks run on the SAME fd (f.Stat must report a regular file, then
// io.ReadAll on that bound fd), so no Lstat→ReadFile window can swap the
// leaf between check and read; a leaf unlinked after open is fine — the fd
// is already bound to the inode. Every failure path (open errno,
// non-regular leaf, read error) returns ok=false — callers degrade to
// their "no pidfile" verdict, never a panic source.
func readRegularPidfile(path string) (raw []byte, ok bool) {
	fd, err := unix.Open(path,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, false
	}
	raw, err = io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return nil, false
	}
	return raw, true
}

// DecodeLock parses a pidfile body. Tolerates legacy plain-decimal pid.
func DecodeLock(raw []byte) *LockInfo {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	var info LockInfo
	if err := json.Unmarshal([]byte(trimmed), &info); err == nil {
		if info.PID > 0 && info.Version != "" {
			return &info
		}
		return nil
	}
	pid, err := strconv.Atoi(trimmed)
	if err != nil || pid <= 0 {
		return nil
	}
	return &LockInfo{PID: pid, Version: "unknown"}
}

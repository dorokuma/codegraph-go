// Package daemon implements the shared MCP daemon (official issue #411 shape):
// one writer process per project root, N thin stdio proxies over a Unix socket.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// PackageVersion is the wire-facing version. Proxy and daemon must match exactly.
const PackageVersion = "0.6.0"

// Soft upper bound for in-project AF_UNIX paths (macOS ~104, Linux ~108).
const posixSocketPathLimit = 100

// LockInfo is the body of .codegraph/daemon.pid.
type LockInfo struct {
	PID        int    `json:"pid"`
	Version    string `json:"version"`
	SocketPath string `json:"socketPath"`
	StartedAt  int64  `json:"startedAt"` // unix ms
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

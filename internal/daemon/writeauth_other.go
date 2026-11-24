//go:build !linux

package daemon

import (
	"net"
	"os"
	"strings"
)

// WriteAuth is the per-connection (or per-process) verdict on whether
// store_fact writes are allowed. On non-Linux platforms the /proc- and
// SO_PEERCRED-based provenance checks cannot run, so the verdict can only
// inspect this process's own environment: explicit read-only markers still
// refuse, while an unmarked developer process keeps normal write access
// (store_fact must stay usable for macOS/Windows development).
type WriteAuth struct {
	Allowed bool
	Reason  string
}

// envTruthyOther mirrors the Linux environTruthy truth-value set
// ("1/true/yes/on", case-insensitive, whitespace-trimmed).
func envTruthyOther(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// selfEnvWriteAuth applies the platform-independent read-only markers to
// the current process's own environment. A process that carries an
// explicit marker denies; anything else allows — off Linux there is no
// ancestry provenance to consult, so the marker set is the only provable
// signal and a clean environment must not break normal development.
func selfEnvWriteAuth() WriteAuth {
	if envTruthyOther(os.Getenv("CTXMODE_READONLY")) {
		return WriteAuth{Allowed: false, Reason: "CTXMODE_READONLY set"}
	}
	if strings.TrimSpace(os.Getenv("PI_ROLE")) != "" {
		return WriteAuth{Allowed: false, Reason: "PI_ROLE set"}
	}
	if v := strings.TrimSpace(os.Getenv("CTXMODE_ROLE")); v != "" && strings.ToLower(v) != "worker" {
		return WriteAuth{Allowed: false, Reason: "CTXMODE_ROLE=" + v}
	}
	if strings.TrimSpace(os.Getenv("PI_SUBAGENT")) == "1" &&
		strings.ToLower(strings.TrimSpace(os.Getenv("CTXMODE_ROLE"))) != "worker" {
		return WriteAuth{Allowed: false, Reason: "PI_SUBAGENT without worker role"}
	}
	return WriteAuth{Allowed: true, Reason: "non-linux: no read-only marker in own environment"}
}

// VerifyClientWriteAuth on non-Linux falls back to the process's own
// environment markers: peer credentials and the /proc ancestry chain are
// Linux-only, so the best provable signal is whether this daemon process
// itself was dispatched read-only.
func VerifyClientWriteAuth(conn net.Conn, chlo ClientHello, helloOK bool) WriteAuth {
	if !helloOK {
		return WriteAuth{Allowed: false, Reason: "missing client hello"}
	}
	return selfEnvWriteAuth()
}

// VerifyDirectWriteAuth applies the same own-environment verdict for a
// direct (non-daemon) stdio process.
func VerifyDirectWriteAuth() WriteAuth {
	return selfEnvWriteAuth()
}

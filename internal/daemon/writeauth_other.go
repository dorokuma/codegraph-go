//go:build !linux

package daemon

import (
	"net"
	"os"
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

// selfEnvWriteAuth applies the platform-independent read-only markers to
// the current process's own environment — the decision logic lives in
// writeauth_selfenv.go (no build tag) so unit tests run on every platform;
// this file only wires os.Getenv.
func selfEnvWriteAuth() WriteAuth {
	return selfEnvWriteAuthFrom(os.Getenv)
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

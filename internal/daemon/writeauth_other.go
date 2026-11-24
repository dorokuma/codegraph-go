//go:build !linux

package daemon

import "net"

// WriteAuth is the per-connection (or per-process) verdict on whether
// store_fact writes are allowed. On non-Linux platforms the /proc- and
// SO_PEERCRED-based provenance checks cannot run, so the conservative
// fail-closed default applies: no write without proof.
type WriteAuth struct {
	Allowed bool
	Reason  string
}

// VerifyClientWriteAuth fails closed off Linux: peer credentials and the
// /proc ancestry chain are Linux-only, and a write we cannot attribute must
// never be silently allowed.
func VerifyClientWriteAuth(conn net.Conn, chlo ClientHello, helloOK bool) WriteAuth {
	if !helloOK {
		return WriteAuth{Allowed: false, Reason: "missing client hello"}
	}
	return WriteAuth{Allowed: false, Reason: "write auth unsupported on this platform"}
}

// VerifyDirectWriteAuth fails closed off Linux for the same reason: a
// direct-mode process without /proc provenance cannot prove it is not a
// read-only role.
func VerifyDirectWriteAuth() WriteAuth {
	return WriteAuth{Allowed: false, Reason: "write auth unsupported on this platform"}
}

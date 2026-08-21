//go:build windows

package daemon

import (
	"net"
)

// listenUnixWithUmask is a stub on Windows where Unix domain sockets and umask are not used.
// Callers on Windows hit SocketCandidates == nil early in Start() and return errNoSocketSupport.
func listenUnixWithUmask(path string) (net.Listener, error) {
	return nil, errNoSocketSupport
}

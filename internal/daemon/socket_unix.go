//go:build !windows

package daemon

import (
	"net"
	"sync"
	"syscall"
)

// umaskMu serializes umask changes across goroutines to prevent races where
// another goroutine creates files with unintended permissions during socket bind.
var umaskMu sync.Mutex

// listenUnixWithUmask binds a Unix domain socket under a temporary 0077 umask,
// ensuring the socket inode is created without group/other access from the moment
// of creation (closing the window before chmod runs).
func listenUnixWithUmask(path string) (net.Listener, error) {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)
	return net.Listen("unix", path)
}

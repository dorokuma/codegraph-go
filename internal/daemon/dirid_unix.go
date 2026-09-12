//go:build unix

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// dirIdentity is the (dev, ino) fingerprint of the .codegraph directory as
// it was when the daemon last verified it. Path-based removals (stale socket
// before bind, pidfile/socket in cleanupArtifacts) compare a fresh stat
// against it: if .codegraph was replaced by a symlink while the daemon ran,
// the fingerprint changes and the removals skip instead of deleting through
// attacker-chosen paths (red team: SIGTERM after a swap removed
// daemon.pid/daemon.sock planted in the symlink target).
type dirIdentity struct {
	dev uint64
	ino uint64
}

// statDirIdentity fingerprints dir with a single stat. The stat follows
// symlinks on purpose: the question is what the PATH resolves to right now,
// not what sits at the path.
func statDirIdentity(dir string) (*dirIdentity, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat %s: did not yield a syscall.Stat_t", dir)
	}
	return &dirIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

// matches reports whether dir still resolves to this identity.
func (id *dirIdentity) matches(dir string) bool {
	cur, err := statDirIdentity(dir)
	return err == nil && cur != nil && *cur == *id
}

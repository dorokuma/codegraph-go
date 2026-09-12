//go:build unix

package cgdir

import (
	"fmt"
	"os"
	"syscall"
)

// statIdentity fingerprints dir with a single stat. The stat follows
// symlinks on purpose: the question is what the PATH resolves to right now,
// not what sits at the path — the same convention as the daemon's
// dirIdentity fingerprint.
func statIdentity(dir string) (Identity, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return Identity{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, fmt.Errorf("stat %s: did not yield a syscall.Stat_t", dir)
	}
	return Identity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

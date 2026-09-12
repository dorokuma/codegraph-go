//go:build unix

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// statPathFn is the path-side stat used by the inode rechecks below. It is a
// variable so tests can deterministically simulate the .codegraph path being
// swapped after the pre-open validation, without racing a real rename inside
// Open.
var statPathFn = os.Stat

// pinnedDir holds the dirfd pinning one validated .codegraph directory.
// Pinning closes the TOCTOU between the path-level validation (Lstat +
// realpath) and the file creations that follow: anything created through the
// pinned fd lands in the inode that was validated, no matter what the path
// resolves to in the meantime (the red team won by polling for
// codegraph.lock and swapping .codegraph for a symlink inside that gap).
type pinnedDir struct {
	fd int // -1 when no pinning is available (degraded mode)
}

// pinDir opens dir with O_DIRECTORY|O_NOFOLLOW and pins the resulting fd.
// O_NOFOLLOW makes the open itself fail when the final path component is a
// symlink, so the fd always refers to a real directory inode resolved within
// this single syscall — the same syscall boundary the path-level checks ran
// just before.
func pinDir(dir string) (*pinnedDir, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("pin .codegraph dir %s: %w", dir, err)
	}
	return &pinnedDir{fd: fd}, nil
}

// close releases the pinned fd. Safe to call more than once.
func (p *pinnedDir) close() error {
	if p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}

// openLockFile creates/opens name relative to the pinned directory.
// O_NOFOLLOW refuses a symlinked lock file instead of following it. The fd
// is wrapped back into an *os.File so the flock semantics are unchanged.
func (p *pinnedDir) openLockFile(dir, name string) (*os.File, error) {
	fullPath := filepath.Join(dir, name)
	if p.fd < 0 {
		return os.OpenFile(fullPath, os.O_CREATE|os.O_RDWR, 0o600)
	}
	fd, err := unix.Openat(p.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("openat %s under pinned .codegraph dir: %w", name, err)
	}
	return os.NewFile(uintptr(fd), fullPath), nil
}

// recheckDir compares the pinned fd against a fresh stat of the path. A
// dev/ino mismatch means the directory behind .codegraph changed after
// validation (e.g. was replaced by a symlink mid-open) and the caller must
// abort before writing anything.
func (p *pinnedDir) recheckDir(dir string) error {
	if p.fd < 0 {
		return nil
	}
	var pinned unix.Stat_t
	if err := unix.Fstat(p.fd, &pinned); err != nil {
		return fmt.Errorf("fstat pinned .codegraph dir: %w", err)
	}
	fi, err := statPathFn(dir)
	if err != nil {
		return fmt.Errorf("stat .codegraph dir %s: %w", dir, err)
	}
	byPath, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify .codegraph dir %s: stat did not yield a syscall.Stat_t", dir)
	}
	if sameDevIno(uint64(pinned.Dev), uint64(pinned.Ino), uint64(byPath.Dev), uint64(byPath.Ino)) {
		return nil
	}
	return fmt.Errorf(".codegraph dir %s changed after validation: the pinned directory (dev=%d ino=%d) no longer matches the path (dev=%d ino=%d); the directory was likely replaced by a symlink mid-open — remove the symlink and re-create the index (codegraph-go init) so the database files stay inside the project", dir, pinned.Dev, pinned.Ino, byPath.Dev, byPath.Ino)
}

// recheckChild stats name relative to the pinned fd (never following
// symlinks) and compares it against a fresh stat of fullPath. Run once after
// the database file has been created: modernc SQLite only receives the path,
// so its own creations re-resolve the path independently of the pin, and
// this comparison detects a swap that happened between the lock recheck and
// SQLite's writes.
func (p *pinnedDir) recheckChild(dir, name, fullPath string) error {
	if p.fd < 0 {
		return nil
	}
	var pinned unix.Stat_t
	if err := unix.Fstatat(p.fd, name, &pinned, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat %s via pinned .codegraph dir: %w", fullPath, err)
	}
	fi, err := statPathFn(fullPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", fullPath, err)
	}
	byPath, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify %s: stat did not yield a syscall.Stat_t", fullPath)
	}
	if sameDevIno(uint64(pinned.Dev), uint64(pinned.Ino), uint64(byPath.Dev), uint64(byPath.Ino)) {
		return nil
	}
	return fmt.Errorf("%s escaped the pinned .codegraph dir: the file inside the pinned directory (dev=%d ino=%d) does not match the path (dev=%d ino=%d); the directory was likely replaced by a symlink after the database was created — remove the symlink and re-create the index (codegraph-go init) so the database files stay inside the project", fullPath, pinned.Dev, pinned.Ino, byPath.Dev, byPath.Ino)
}

// sameDevIno reports whether two (dev, ino) pairs describe the same on-disk
// object. Dev/Ino field types differ per platform, so the values are widened
// to uint64 by the callers.
func sameDevIno(aDev, aIno, bDev, bIno uint64) bool {
	return aDev == bDev && aIno == bIno
}

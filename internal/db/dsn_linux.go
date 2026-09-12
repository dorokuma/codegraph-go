//go:build linux

package db

import (
	"fmt"
	"os"
	"path/filepath"
)

// pinnedDBPath returns the path SQLite opens codegraph.db through.
//
// Linux: re-expose the pinned directory fd via /proc/self/fd/<fd>. A procfs
// magic link always resolves to the inode the fd refers to, so every path
// resolution SQLite performs — creating and reopening codegraph.db, -wal and
// -shm — lands in the directory that passed validation, no matter what the
// .codegraph path resolves to in the meantime. This closes the residual
// TOCTOU the 0.9.7 defense only detected (SQLite received dbPath by value,
// so its own opens re-resolved the path and a swap after the lock recheck
// moved the db file outside). The pinned fd is held for the whole connection
// lifetime (DB.pinned) and released in Close, keeping the DSN valid until
// the last SQLite handle is gone.
//
// Fallback to the validated plain path when the magic link does not resolve
// (procfs not mounted, e.g. some chroots/sandboxes): there the recheck lines
// in pin_unix.go remain the detection layer, as on non-Linux platforms.
func pinnedDBPath(p *pinnedDir, dir string) string {
	if p != nil && p.fd >= 0 {
		procDir := fmt.Sprintf("/proc/self/fd/%d", p.fd)
		// Pre-flight: use the magic link only when it actually resolves to
		// a directory (the stat follows the link). Otherwise fall back to
		// the plain path below.
		if fi, err := os.Stat(procDir); err == nil && fi.IsDir() {
			return procDir + "/codegraph.db"
		}
	}
	return filepath.Join(dir, "codegraph.db")
}

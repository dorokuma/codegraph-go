//go:build linux

package db

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// AllowPlainDBEnv is the escape hatch for environments that genuinely have
// no usable procfs (some chroots/sandboxes): when it is set to "1", a
// failing /proc/self/fd/<fd> probe falls back to the validated plain path
// instead of failing Open, deliberately downgrading the TOCTOU defense from
// prevention to detection (the recheckDir/recheckChild lines in pin_unix.go
// remain the detection layer).
const AllowPlainDBEnv = "CODEGRAPH_ALLOW_PLAIN_DB"

// procFdStatFn probes whether the /proc/self/fd/<fd> magic link resolves.
// It is a variable so tests can simulate an unavailable procfs; production
// behavior always uses the real os.Stat.
var procFdStatFn = os.Stat

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
// Fail closed: when the magic link does not resolve to a directory (procfs
// not mounted, shadowed, or otherwise unusable), Open fails with an explicit
// error instead of silently falling back to the plain path — there the TOCTOU
// defense would degrade to the recheck-only detection of pin_unix.go, which
// lets an attacker race the db files out of the project before the recheck
// notices. Set CODEGRAPH_ALLOW_PLAIN_DB=1 to keep the historical fallback
// (with a logged warning) on hosts that cannot mount procfs.
func pinnedDBPath(p *pinnedDir, dir string) (string, error) {
	if p != nil && p.fd >= 0 {
		procDir := fmt.Sprintf("/proc/self/fd/%d", p.fd)
		usable, probeErr := procfsMagicLinkUsable(procDir)
		if usable {
			return procDir + "/codegraph.db", nil
		}
		plain := filepath.Join(dir, "codegraph.db")
		if os.Getenv(AllowPlainDBEnv) == "1" {
			log.Printf("db: %v; %s=1 set, opening codegraph.db through the plain path %q (dirfd TOCTOU defense degraded to detection only)", probeErr, AllowPlainDBEnv, plain)
			log.Printf("db: plain-path DSN in use (degraded): directory swaps are detected by recheck checks, not prevented")
			return plain, nil
		}
		return "", fmt.Errorf("pinned .codegraph dir: %v; refusing to open codegraph.db through the plain path because the dirfd TOCTOU defense needs a resolvable /proc/self/fd magic link (mount procfs, or set %s=1 to accept the degraded detection-only fallback)", probeErr, AllowPlainDBEnv)
	}
	// No pin (nil or degraded fd): as before, the validated plain path.
	return filepath.Join(dir, "codegraph.db"), nil
}

// procfsMagicLinkUsable reports whether the /proc/self/fd/<fd> magic link
// currently resolves to a directory (the stat follows the link). Any failure
// — unreadable procfs, or a link resolving to a non-directory — makes the
// magic-link DSN unusable.
func procfsMagicLinkUsable(procDir string) (bool, error) {
	fi, err := procFdStatFn(procDir)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w (procfs unavailable?)", procDir, err)
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s does not resolve to a directory", procDir)
	}
	return true, nil
}

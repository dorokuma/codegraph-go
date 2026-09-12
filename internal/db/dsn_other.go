//go:build !linux

package db

import "path/filepath"

// pinnedDBPath returns the path SQLite opens codegraph.db through.
//
// Non-Linux fallback: /proc/self/fd magic links are a Linux procfs feature
// (macOS and the BSDs have no equivalent), so SQLite keeps receiving the
// validated plain path. The dirfd pin and the recheckDir/recheckChild lines
// in pin_unix.go stay the detection layer there, exactly as before. The
// error is always nil here: the fail-closed procfs behavior (and its
// CODEGRAPH_ALLOW_PLAIN_DB escape hatch, see dsn_linux.go) guards the
// magic-link DSN, which only exists on Linux — this platform never had a
// stronger defense to lose.
func pinnedDBPath(_ *pinnedDir, dir string) (string, error) {
	return filepath.Join(dir, "codegraph.db"), nil
}

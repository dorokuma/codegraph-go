//go:build !linux

package db

import "path/filepath"

// pinnedDBPath returns the path SQLite opens codegraph.db through.
//
// Non-Linux fallback: /proc/self/fd magic links are a Linux procfs feature
// (macOS and the BSDs have no equivalent), so SQLite keeps receiving the
// validated plain path. The dirfd pin and the recheckDir/recheckChild lines
// in pin_unix.go stay the detection layer there, exactly as before.
func pinnedDBPath(_ *pinnedDir, dir string) string {
	return filepath.Join(dir, "codegraph.db")
}

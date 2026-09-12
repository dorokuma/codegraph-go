//go:build !unix

package db

import (
	"os"
	"path/filepath"
)

// Non-unix fallback: golang.org/x/sys/unix and *syscall.Stat_t are not
// available there, so the dirfd pinning degrades to the path-level checks
// (Lstat + realpath) that Open already performs before creating anything.
// The rechecks become no-ops instead of failing: pinning is a hardening
// layer on top of the path-level jail, and on these platforms the guard
// stays exactly as strong as it was before this change. The fallback is
// theoretical anyway: the repository as a whole does not build on non-unix
// platforms (including windows) in the first place — the tree-sitter cgo
// dependency has no non-unix support upstream and CI has no coverage there
// — and that was already true before 0.9.5, so this file degrades nothing
// that ever worked.

type pinnedDir struct{}

func pinDir(dir string) (*pinnedDir, error) { return &pinnedDir{}, nil }

func (p *pinnedDir) close() error { return nil }

func (p *pinnedDir) openLockFile(dir, name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600)
}

func (p *pinnedDir) recheckDir(dir string) error { return nil }

func (p *pinnedDir) recheckChild(dir, name, fullPath string) error { return nil }

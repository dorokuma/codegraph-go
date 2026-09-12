// Package cgdir is the shared guard for a project's .codegraph directory
// (the symlink jail): every code path that creates or writes files under
// .codegraph must run Ensure first. A symlinked .codegraph turns file
// creation into writes through the link, landing daemon.log, daemon.pid,
// daemon.sock and codegraph.db/-wal/-shm/lock at the symlink TARGET —
// outside the project. Ensure refuses such a directory up front, creates a
// fresh real one when it is missing, and re-verifies the realpath after
// creation so the jail holds on both sides of the mkdir.
//
// The package deliberately depends only on os/filepath/fmt: it is a leaf,
// importable from db and daemon alike, and stays free of policy. The
// per-file TOCTOU hardening (dirfd pinning) lives with its single consumer
// in internal/db.
package cgdir

import (
	"fmt"
	"os"
	"path/filepath"
)

// Ensure makes dir (the project's .codegraph) safe to write into, failing
// closed on anything that would send the writes outside the project:
//
//  1. Lstat line — an existing .codegraph that is a symlink (or not a real
//     directory) is rejected before anything is created or modified. A
//     missing .codegraph passes; Ensure creates a fresh real directory next.
//  2. MkdirAll — create the directory (no-op when it already exists).
//  3. realpath line — after creation, .codegraph must still resolve to
//     itself, or Ensure fails before a single file is written inside it.
//     Only the .codegraph component itself may not alias elsewhere — a
//     symlinked ANCESTOR stays allowed, because the codebase treats a
//     symlinked workdir as a supported alias of its real path (see
//     db.PathUnderRoot and db.FindNearestCodeGraphRoot, which resolve/skip
//     instead of reject); the parent is therefore resolved separately
//     before comparing.
//
// Callers must fail closed: when Ensure returns an error, nothing may be
// written under dir.
//
// On success Ensure also returns the (dev, ino) Identity of the directory it
// just verified, stat'ed immediately after the realpath line. Callers that
// fingerprint .codegraph for later swap detection (daemon.Start's dirID)
// take it from here instead of stat'ing the path again — a second stat
// after Ensure returns would re-open the microsecond swap window the jail
// just closed.
func Ensure(dir string) (Identity, error) {
	if err := rejectSymlinkDir(dir); err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, fmt.Errorf("create .codegraph dir: %w", err)
	}
	if err := verifyRealPath(dir); err != nil {
		return Identity{}, err
	}
	// Fingerprint the just-verified directory right away: this stat IS the
	// identity callers record, taken while the jail checks above are still
	// warm. A separate stat after Ensure returns would re-open the
	// microsecond swap window between verification and fingerprinting.
	return statIdentity(dir)
}

// Identity is the (dev, ino) fingerprint of a .codegraph directory as it
// resolved when Ensure verified it. cgdir is a leaf package (imported by db
// and daemon alike) and cannot import the daemon's dirid helpers, so it
// carries the two raw numbers; the daemon converts them to its own type. On
// platforms without a syscall.Stat_t the fingerprint is the zero value and
// fingerprinting degrades to "no identity" (the daemon mirrors this with
// its dirid_other.go fallback).
type Identity struct {
	Dev uint64
	Ino uint64
}

// rejectSymlinkDir is the Lstat line of the .codegraph symlink jail: it
// refuses to continue when <workdir>/.codegraph is a symlink (its target is
// not this project's index, and creating files there would write outside
// the project) or not a directory at all. A missing .codegraph passes —
// Ensure creates a fresh real directory right after.
func rejectSymlinkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat .codegraph dir %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(".codegraph is a symlink; refusing to open %s: remove the symlink and re-create the index (codegraph-go init) so the database files stay inside the project", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf(".codegraph is not a directory: %s", dir)
	}
	return nil
}

// verifyRealPath is the realpath line of the .codegraph symlink jail: the
// directory must resolve to itself, otherwise nothing may be written through
// the path. Only the .codegraph component itself may not alias elsewhere —
// a symlinked ANCESTOR stays allowed, because the codebase treats a
// symlinked workdir as a supported alias of its real path (see
// db.PathUnderRoot and db.FindNearestCodeGraphRoot, which resolve/skip
// instead of reject); the parent is therefore resolved separately before
// comparing.
func verifyRealPath(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve .codegraph dir %s: %w", dir, err)
	}
	want := filepath.Join(filepath.Dir(dir), filepath.Base(dir))
	if realParent, perr := filepath.EvalSymlinks(filepath.Dir(dir)); perr == nil && realParent != "" {
		want = filepath.Join(realParent, filepath.Base(dir))
	}
	if real != want {
		return fmt.Errorf(".codegraph is a symlink; refusing to open %s: it resolves to %s", dir, real)
	}
	return nil
}

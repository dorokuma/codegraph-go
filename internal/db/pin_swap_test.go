//go:build linux

package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenWritesViaMagicLinkAfterSwap is the regression test for the 0.9.7
// residual the red team proved 20/20: the old defense only DETECTED a swap
// after the lock recheck (recheckChild rc=1) while SQLite had already been
// handed codegraph.db as a PATH — the db file itself escaped to the symlink
// target. With the procfs magic-link DSN the connection is bound to the
// pinned directory inode for its whole lifetime, so a swap AFTER Open must
// not move a single byte: data written through the open DB lands in the
// real (renamed) directory and the symlink target stays empty.
func TestOpenWritesViaMagicLinkAfterSwap(t *testing.T) {
	proj := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}

	database, err := Open(proj)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	cg := filepath.Join(proj, ".codegraph")
	// The swap, made after Open is fully done: real directory away, a
	// symlink to the outside victim in its place.
	if err := os.Rename(cg, cg+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, cg); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })

	// Write AFTER the swap, through the already-open DB.
	if _, err := database.conn.Exec(`INSERT INTO meta(key, value) VALUES('swap-probe', 'x')`); err != nil {
		t.Fatalf("write after swap: %v", err)
	}

	// The bytes must be in the REAL directory's database file: read it with
	// a fresh, independent SQLite handle on the renamed path (WAL frames are
	// recovered on open, so nothing needs to be checkpointed first).
	conn, err := sql.Open("sqlite", filepath.Join(cg+".real", "codegraph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM meta WHERE key='swap-probe'`).Scan(&n); err != nil {
		t.Fatalf("read real db after swap: %v", err)
	}
	if n != 1 {
		t.Fatalf("post-swap write not found in the real directory's db (hits=%d)", n)
	}

	// The symlink target must have gained nothing.
	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target polluted, want zero entries, got: %v", entries)
	}
}

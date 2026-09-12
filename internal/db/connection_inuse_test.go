package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestOpenWrapsErrIndexInUse: when another process (here: a foreign fd in
// this test process, equivalent for flock semantics) holds the A1
// single-writer lock, Open must fail with an error matching
// errors.Is(err, ErrIndexInUse) so callers detect the conflict without
// string matching, while the rendered message keeps the historical wording
// (cmd canonicalizes it into the actionable "database in use" error).
func TestOpenWrapsErrIndexInUse(t *testing.T) {
	dir := t.TempDir()
	lockDir := filepath.Join(dir, ".codegraph")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(filepath.Join(lockDir, "codegraph.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Skipf("could not take flock in this environment: %v", err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	db, err := Open(dir)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open succeeded while the single-writer lock was held")
	}
	if !errors.Is(err, ErrIndexInUse) {
		t.Fatalf("Open error does not match ErrIndexInUse: %v", err)
	}
	if !strings.Contains(err.Error(), "codegraph.db in use by another process") {
		t.Fatalf("in-use wording lost: %v", err)
	}
}

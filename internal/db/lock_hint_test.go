package db

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestLockHolderHintSelfReferenceGuard: when daemon.pid names the CURRENT
// process, lockHolderHint must return "" — a daemon writes its own pidfile
// before opening the DB, so during an invisible-holder wedge the recorded
// pid is the reporting process itself and "held by pid <self>" would be a
// lie about who owns the flock.
func TestLockHolderHintSelfReferenceGuard(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "daemon.pid")

	if err := os.WriteFile(pidfile, []byte(fmt.Sprintf(`{"pid": %d, "version": "0.8.8"}`, os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if hint := lockHolderHint(dir); hint != "" {
		t.Fatalf("self-referencing pidfile must yield no hint, got %q", hint)
	}
}

// TestLockHolderHintOtherProcess: when daemon.pid names a DIFFERENT live
// process (the normal case), the hint carries pid and version.
func TestLockHolderHintOtherProcess(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "daemon.pid")

	if err := os.WriteFile(pidfile, []byte(`{"pid": 4242, "version": "0.8.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := " (held by pid 4242, version 0.8.1)"
	if hint := lockHolderHint(dir); hint != want {
		t.Fatalf("hint = %q, want %q", hint, want)
	}

	// Without a version the suffix is omitted.
	if err := os.WriteFile(pidfile, []byte(`{"pid": 4242}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if hint := lockHolderHint(dir); hint != " (held by pid 4242)" {
		t.Fatalf("hint without version = %q", hint)
	}
}

// TestLockHolderHintMissingOrCorrupt: no pidfile, empty pidfile, or a
// non-JSON body must all yield "".
func TestLockHolderHintMissingOrCorrupt(t *testing.T) {
	dir := t.TempDir()

	if hint := lockHolderHint(dir); hint != "" {
		t.Fatalf("missing pidfile must yield no hint, got %q", hint)
	}

	pidfile := filepath.Join(dir, "daemon.pid")
	for _, body := range []string{"", "not json at all", `{"pid": 0}`, `{"pid": -1}`, `{"version": "0.8.1"}`} {
		if err := os.WriteFile(pidfile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if hint := lockHolderHint(dir); hint != "" {
			t.Fatalf("corrupt pidfile %q must yield no hint, got %q", body, hint)
		}
	}
}

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/daemon"
)

// TestDBInUseErrorWording: the canonical "database in use" rewrite keeps one
// wording everywhere, but the CODEGRAPH_NO_DAEMON hint must be dropped in
// NO_DAEMON direct mode where it would be self-contradictory (G2).
func TestDBInUseErrorWording(t *testing.T) {
	base := errors.New("codegraph.db in use by another process: lock held by another process")

	// Direct-fallback context: full hint is appropriate.
	withHint := dbInUseError(base, true)
	if withHint == nil || !strings.Contains(withHint.Error(), "set CODEGRAPH_NO_DAEMON=1") {
		t.Fatalf("fallback wording missing CODEGRAPH_NO_DAEMON hint: %v", withHint)
	}
	if !strings.Contains(withHint.Error(), "stop the other process first") {
		t.Fatalf("fallback wording missing actionable guidance: %v", withHint)
	}
	if !strings.Contains(withHint.Error(), base.Error()) {
		t.Fatalf("fallback wording dropped the underlying cause: %v", withHint)
	}

	// NO_DAEMON context: the hint is already in effect and must not be echoed.
	noHint := dbInUseError(base, false)
	if noHint == nil || strings.Contains(noHint.Error(), "CODEGRAPH_NO_DAEMON") {
		t.Fatalf("NO_DAEMON wording must not suggest CODEGRAPH_NO_DAEMON: %v", noHint)
	}
	if !strings.Contains(noHint.Error(), "stop the other process first") {
		t.Fatalf("NO_DAEMON wording missing actionable guidance: %v", noHint)
	}

	// Non in-use errors pass through untouched.
	other := errors.New("disk full")
	if got := dbInUseError(other, true); got != other {
		t.Fatalf("non in-use error was rewritten: %v", got)
	}
	if dbInUseError(nil, true) != nil {
		t.Fatal("nil error must stay nil")
	}
}

// TestCheckDirectFallbackSafeLivePidfile: the pidfile-live branch routes
// through the shared dbInUseMessage helper — same canonical wording (with the
// CODEGRAPH_NO_DAEMON hint, since this is the daemon-fallback path) and the
// daemon pid preserved (G2).
func TestCheckDirectFallbackSafeLivePidfile(t *testing.T) {
	root := t.TempDir()
	pidPath := daemon.PidPath(root)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pidfile naming a live process (ourselves) must block the direct
	// fallback — running direct would double-write the index.
	info := daemon.LockInfo{PID: os.Getpid(), Version: "0.0.0", SocketPath: "", StartedAt: 1}
	if err := os.WriteFile(pidPath, daemon.EncodeLock(info), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkDirectFallbackSafe(root)
	if err == nil {
		t.Fatal("direct fallback allowed while a live daemon pidfile exists")
	}
	msg := err.Error()
	if !strings.Contains(msg, "daemon pid") || !strings.Contains(msg, "stop the other process first") {
		t.Fatalf("pidfile branch did not use the shared wording: %v", msg)
	}
	if !strings.Contains(msg, "set CODEGRAPH_NO_DAEMON=1") {
		t.Fatalf("daemon-fallback wording missing CODEGRAPH_NO_DAEMON hint: %v", msg)
	}
}

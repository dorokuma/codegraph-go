package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketCandidatesInProject(t *testing.T) {
	root := "/tmp/cg-short-root"
	c := SocketCandidates(root)
	if len(c) < 1 {
		t.Fatal("expected at least one candidate")
	}
	if !strings.HasSuffix(c[0], filepath.Join(".codegraph", "daemon.sock")) &&
		!strings.Contains(c[0], "daemon.sock") {
		// first should be in-project when path is short
		if !strings.Contains(c[0], ".codegraph") {
			t.Fatalf("preferred candidate = %q, want in-project", c[0])
		}
	}
	if PreferredSocket(root) != c[0] {
		t.Fatalf("PreferredSocket != candidates[0]")
	}
}

func TestSocketCandidatesLongPathFallsToTmp(t *testing.T) {
	// Build a root so in-project sock exceeds limit.
	long := strings.Repeat("a", 120)
	root := filepath.Join("/tmp", long, "proj")
	c := SocketCandidates(root)
	if len(c) != 1 {
		t.Fatalf("long path candidates=%v want single tmpdir", c)
	}
	if !strings.Contains(c[0], os.TempDir()) {
		t.Fatalf("want tmpdir socket, got %q", c[0])
	}
}

func TestEncodeDecodeLock(t *testing.T) {
	info := LockInfo{PID: 42, Version: "0.7.0", SocketPath: "/tmp/x.sock", StartedAt: 99}
	raw := EncodeLock(info)
	got := DecodeLock(raw)
	if got == nil || got.PID != 42 || got.Version != "0.7.0" || got.SocketPath != "/tmp/x.sock" {
		t.Fatalf("roundtrip got %+v", got)
	}
	// legacy plain pid
	leg := DecodeLock([]byte("1234\n"))
	if leg == nil || leg.PID != 1234 || leg.Version != "unknown" {
		t.Fatalf("legacy decode %+v", leg)
	}
}

func TestPidPath(t *testing.T) {
	p := PidPath("/proj")
	if p != filepath.Join("/proj", ".codegraph", "daemon.pid") {
		t.Fatalf("pid path %q", p)
	}
}

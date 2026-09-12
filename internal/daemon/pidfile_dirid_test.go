package daemon

import (
	"os"
	"strings"
	"testing"
)

// The pidfile records the (dev, ino) identity of the .codegraph directory at
// acquire time so a same-inode swap (rename + symlink pointing back at the
// original inode) - which passes every existing guard with integrity intact -
// still leaves an audit trail in the pidfile and the daemon.log start line.
// The fields are observability only: they never influence the kill identity
// checks.

// TestTryAcquireLockRecordsDirIdentity: a freshly acquired lock carries the
// .codegraph directory identity, both in the returned LockInfo and in the
// on-disk pidfile JSON, and survives an encode/decode roundtrip.
func TestTryAcquireLockRecordsDirIdentity(t *testing.T) {
	root := t.TempDir()
	cg := CodeGraphDir(root)
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	t.Cleanup(func() { _ = os.Remove(res.PidPath) })

	id, err := statDirIdentity(cg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Info.DirDev != id.dev || res.Info.DirIno != id.ino {
		t.Fatalf("LockInfo dir identity dev=%d ino=%d, want dev=%d ino=%d",
			res.Info.DirDev, res.Info.DirIno, id.dev, id.ino)
	}
	if res.Info.DirDev == 0 || res.Info.DirIno == 0 {
		t.Fatal("dir identity fields must be populated on unix")
	}

	raw, err := os.ReadFile(res.PidPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"dirDev"`) || !strings.Contains(string(raw), `"dirIno"`) {
		t.Fatalf("pidfile must carry the dir identity fields, got:\n%s", raw)
	}
	back := DecodeLock(raw)
	if back == nil || back.DirDev != id.dev || back.DirIno != id.ino {
		t.Fatalf("pidfile roundtrip lost the dir identity: %+v", back)
	}
}

// TestDecodeLockToleratesPidfileWithoutDirIdentity: pidfiles written by
// older builds (no dirDev/dirIno keys) and the legacy bare-pid format still
// decode, with the new fields defaulting to zero.
func TestDecodeLockToleratesPidfileWithoutDirIdentity(t *testing.T) {
	raw := `{
  "pid": 4242,
  "version": "0.9.8",
  "socketPath": "/proj/.codegraph/daemon.sock",
  "startedAt": 1700000000000,
  "procStart": 123456
}`
	info := DecodeLock([]byte(raw))
	if info == nil {
		t.Fatal("old-format JSON pidfile must decode")
	}
	if info.PID != 4242 || info.Version != "0.9.8" || info.ProcStart != 123456 {
		t.Fatalf("old-format pidfile decoded wrong: %+v", info)
	}
	if info.DirDev != 0 || info.DirIno != 0 {
		t.Fatalf("absent dir identity must decode as zero: %+v", info)
	}

	// Legacy plain-decimal pidfile: unchanged behavior.
	legacy := DecodeLock([]byte("42\n"))
	if legacy == nil || legacy.PID != 42 || legacy.DirDev != 0 || legacy.DirIno != 0 {
		t.Fatalf("legacy pidfile decoded wrong: %+v", legacy)
	}
}

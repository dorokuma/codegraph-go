package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRegistryDirEmptyWhenHomeUnresolvable: without a resolvable home
// directory the registry must be disabled (""), NOT fall back to
// os.TempDir() — writing daemon discovery records into /tmp/.codegraph is
// exactly the walk-up misidentification pollution the .codegraph guards
// elsewhere exist to prevent.
func TestRegistryDirEmptyWhenHomeUnresolvable(t *testing.T) {
	t.Setenv("HOME", "")
	if got := RegistryDir(); got != "" {
		t.Fatalf("RegistryDir with unresolvable home = %q, want an empty string", got)
	}
}

// TestRegistrySkippedWhenHomeUnresolvable: with home unresolvable, Register,
// Deregister and List must be no-ops — nothing may be written into the
// (attacker-influenceable) temp directory or, worse, relative to the process
// working directory (an empty RegistryDir leaking into a record path must
// never produce a relative path).
func TestRegistrySkippedWhenHomeUnresolvable(t *testing.T) {
	t.Setenv("HOME", "")
	cwd := t.TempDir()
	t.Chdir(cwd)

	root := t.TempDir()
	Register(Record{Root: root, PID: 1, Version: "0.0.0", SocketPath: "/dev/null", StartedAt: 1})
	Deregister(root)
	if recs := List(); recs != nil {
		t.Fatalf("List with unresolvable home = %v, want nil", recs)
	}

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); ext == ".json" || ext == ".tmp" {
			t.Fatalf("registry I/O with unresolvable home wrote %s into the working directory", e.Name())
		}
	}
}

// TestRegisterWritesUnderHome (guard against overcorrecting): with a normal
// home the registry still works and lands under $HOME/.codegraph/daemons.
func TestRegisterWritesUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()

	Register(Record{Root: root, PID: 1, Version: "0.0.0", SocketPath: "/dev/null", StartedAt: 1})
	defer Deregister(root) //nolint:errcheck

	if got := RegistryDir(); got != filepath.Join(home, ".codegraph", "daemons") {
		t.Fatalf("RegistryDir = %q, want %q", got, filepath.Join(home, ".codegraph", "daemons"))
	}
	if _, err := os.Stat(filepath.Join(home, ".codegraph", "daemons")); err != nil {
		t.Fatalf("registry directory not created under home: %v", err)
	}
}

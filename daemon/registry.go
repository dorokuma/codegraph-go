package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Record is a discovery entry under ~/.codegraph/daemons/ (best-effort).
type Record struct {
	Root       string `json:"root"`
	PID        int    `json:"pid"`
	Version    string `json:"version"`
	SocketPath string `json:"socketPath"`
	StartedAt  int64  `json:"startedAt"`
}

// RegistryDir is ~/.codegraph/daemons.
func RegistryDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".codegraph", "daemons")
}

func recordPath(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	h := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(RegistryDir(), h+".json")
}

// Register writes a discovery record (best-effort).
// Uses temp+rename so a pre-existing symlink cannot trick us into
// overwriting an unrelated file.
func Register(rec Record) {
	dir := RegistryDir()
	_ = os.MkdirAll(dir, 0o700)
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	payload := append(b, '\n')
	target := recordPath(rec.Root)
	// If a symlink is already sitting at target, remove it directly
	// (os.Remove follows symlinks, but Lstat tells us what we're removing).
	if fi, lerr := os.Lstat(target); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(target)
	}
	// Write to a temp file in the same directory, then atomically rename.
	// On the same filesystem rename(2) does NOT follow symlinks — it
	// replaces the dentry, so even if target reappears as a symlink between
	// Lstat and Rename, we are safe.
	tmp := target + ".tmp"
	if werr := os.WriteFile(tmp, payload, 0o600); werr != nil {
		return
	}
	_ = os.Rename(tmp, target)
	// Best-effort cleanup of stale tmp file on rename failure (e.g. cross-device).
	_ = os.Remove(tmp)
}

// Deregister removes the discovery record (best-effort).
// Refuses to follow a symlink so an attacker can't trick us into deleting
// an unrelated file.
func Deregister(root string) {
	path := recordPath(root)
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		// Symlink at the record path — refuse to follow it.
		return
	}
	_ = os.Remove(path)
}

// List returns live registered daemons, newest first. Dead records are pruned.
func List() []Record {
	dir := RegistryDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var live []Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var rec Record
		if json.Unmarshal(raw, &rec) != nil || rec.PID <= 0 || rec.Root == "" {
			_ = os.Remove(full)
			continue
		}
		if IsProcessAlive(rec.PID) {
			live = append(live, rec)
		} else {
			_ = os.Remove(full)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].StartedAt > live[j].StartedAt })
	return live
}

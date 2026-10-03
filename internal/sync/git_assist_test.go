package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedGitEnv builds the environment for the throwaway git repositories
// these tests create. Pointing GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM at
// /dev/null makes git read an EMPTY global/system config, so a host-global
// core.hooksPath (a commit-msg hook that rejects non-conventional messages)
// or init.templatedir cannot bleed into the repo. Any incoming copies of
// these variables are dropped first so the override always wins: the
// isolation must not depend on the host (or a deliberately hostile one)
// happening to leave them unset.
func isolatedGitEnv() []string {
	const (
		globalKey = "GIT_CONFIG_GLOBAL"
		systemKey = "GIT_CONFIG_SYSTEM"
	)
	base := os.Environ()
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if key, _, ok := strings.Cut(kv, "="); ok && (key == globalKey || key == systemKey) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, globalKey+"=/dev/null", systemKey+"=/dev/null")
}

func TestGitDirtySourceFiles(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		cmd.Env = isolatedGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s (%v)", args, out, err)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@t.com")
	run("git", "config", "user.name", "t")

	src := filepath.Join(root, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "main.go")
	run("git", "commit", "-m", "init")

	// Clean tree → empty
	if got := GitDirtySourceFiles(root); len(got) != 0 {
		t.Fatalf("clean dirty=%v", got)
	}

	// Modify
	if err := os.WriteFile(src, []byte("package main\nfunc main() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := GitDirtySourceFiles(root)
	if len(got) != 1 || filepath.Base(got[0]) != "main.go" {
		t.Fatalf("dirty=%v", got)
	}

	// Unsupported file ignored
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "readme.md")
	// still only main.go dirty as content change; readme is staged new — may appear
	// porcelain "A  readme.md" — filtered by IsSupported
	got = GitDirtySourceFiles(root)
	for _, f := range got {
		if filepath.Ext(f) == ".md" {
			t.Fatalf("md should be filtered: %v", got)
		}
	}
}

func TestGitDirtyNonRepo(t *testing.T) {
	if got := GitDirtySourceFiles(t.TempDir()); got != nil {
		t.Fatalf("non-repo: %v", got)
	}
}

// TestGitDirtySourceFilesRename verifies that a git rename surfaces BOTH the
// old path (to drop from the index) and the new path (to index).
func TestGitDirtySourceFilesRename(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		cmd.Env = isolatedGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s (%v)", args, out, err)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@t.com")
	run("git", "config", "user.name", "t")

	src := filepath.Join(root, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "main.go")
	run("git", "commit", "-m", "init")

	dst := filepath.Join(root, "renamed.go")
	run("git", "mv", "main.go", "renamed.go")

	got := GitDirtySourceFiles(root)
	hasOld, hasNew := false, false
	for _, f := range got {
		if filepath.Clean(f) == filepath.Clean(src) {
			hasOld = true
		}
		if filepath.Clean(f) == filepath.Clean(dst) {
			hasNew = true
		}
	}
	if !hasOld {
		t.Errorf("rename old path missing from dirty set: %v", got)
	}
	if !hasNew {
		t.Errorf("rename new path missing from dirty set: %v", got)
	}
}

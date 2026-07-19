package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitDirtySourceFiles(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
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

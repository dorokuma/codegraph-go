package server

import (
	"strings"
	"testing"
)

func TestTruncateOutput(t *testing.T) {
	short := "hello"
	if truncateOutput(short, 18_000) != short {
		t.Fatal("short text should pass through")
	}
	long := strings.Repeat("a", 20_000)
	out := truncateOutput(long, 100)
	if !strings.HasPrefix(out, strings.Repeat("a", 100)) {
		t.Fatal("prefix should match")
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("should mention truncated")
	}
}

func TestLimitLines(t *testing.T) {
	in := "a\nb\nc\nd\n"
	out := limitLines(in, 2)
	want := "a\nb\n... (max 2 lines; narrow path/glob or raise max_results)"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestSearchPerFileCap(t *testing.T) {
	if searchPerFileCap(70) != defaultSearchPerFile {
		t.Fatal("expected default per-file")
	}
	if searchPerFileCap(5) != 5 {
		t.Fatal("global below per-file should shrink per-file")
	}
}

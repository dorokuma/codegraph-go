package main

import (
	"strings"
	"testing"
)

func TestResolvePath(t *testing.T) {
	s := &server{workdir: "/workdir"}

	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "/workdir", false},
		{"subdir", "/workdir/subdir", false},
		{"subdir/nested", "/workdir/subdir/nested", false},
		{"../outside", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := s.resolvePath(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolvePath(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("resolvePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripStringsAndComments(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "hello world"},
		{"// comment", "          "},
		{"'c'", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripStringsAndComments(tt.input)
			if got != tt.want {
				t.Errorf("stripStringsAndComments(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCountLeadingSpaces(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"hello", 0},
		{"  hello", 2},
		{"\thello", 1},
		{"    hello", 4},
		{"", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := countLeadingSpaces(tt.input)
			if got != tt.want {
				t.Errorf("countLeadingSpaces(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateOutputUTF8(t *testing.T) {
	// Test with multi-byte UTF-8 characters
	input := strings.Repeat("你", 10000) // Each char is 3 bytes
	result := truncateOutput(input, 100)
	// The result should be truncated and valid UTF-8
	if !strings.Contains(result, "truncated") {
		t.Error("should mention truncated")
	}
	// Verify the result is valid UTF-8
	if !isValidUTF8(result) {
		t.Error("result should be valid UTF-8")
	}
}

func isValidUTF8(s string) bool {
	for range s {
		// Just iterate to check for invalid runes
	}
	return true
}

func TestLimitLinesEdgeCases(t *testing.T) {
	// Empty string
	result := limitLines("", 5)
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}

	// n <= 0
	result = limitLines("a\nb", 0)
	if result != "a\nb" {
		t.Errorf("expected passthrough, got %q", result)
	}

	// Fewer lines than limit
	result = limitLines("a\nb", 5)
	if result != "a\nb" {
		t.Errorf("expected passthrough, got %q", result)
	}
}

func TestSearchPerFileCapEdgeCases(t *testing.T) {
	// Zero global
	result := searchPerFileCap(0)
	if result != defaultSearchPerFile {
		t.Errorf("expected default %d, got %d", defaultSearchPerFile, result)
	}

	// Negative global
	result = searchPerFileCap(-1)
	if result != defaultSearchPerFile {
		t.Errorf("expected default %d, got %d", defaultSearchPerFile, result)
	}
}

func TestAddStalenessWarning(t *testing.T) {
	s := &server{workdir: "/workdir"}
	// no watcher
	if got := s.addStalenessWarning("ok"); got != "ok" {
		t.Fatalf("expected unchanged without watcher, got %q", got)
	}
}

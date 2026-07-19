package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Mid-point defaults: small enough for SSH / token budgets, large enough
// that a coding agent rarely needs a second round for normal navigation.
const (
	// Hard cap on any single tool payload returned over MCP.
	defaultOutputChars = 18_000

	// search: max_results is the global match cap; per-file is a separate ceiling.
	defaultSearchGlobal  = 70
	defaultSearchPerFile = 12

	// files: path listing default
	defaultFilesMax = 100

	// callers / callees / impact style tools
	defaultSymbolMax = 40
)

// truncateOutput cuts text to max bytes on a UTF-8 boundary and appends a hint.
func truncateOutput(text string, max int) string {
	if max <= 0 {
		max = defaultOutputChars
	}
	if len(text) <= max {
		return text
	}
	truncAt := max
	for truncAt > 0 && !utf8.ValidString(text[:truncAt]) {
		truncAt--
	}
	return text[:truncAt] + "\n... (truncated; narrow path/glob or raise max_results)"
}

// limitLines keeps the first n non-empty lines (or all if n <= 0).
// If truncated, appends a short note with the cap.
func limitLines(text string, n int) string {
	if n <= 0 || text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	// Drop a single trailing empty line from Split of trailing newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= n {
		return text
	}
	kept := lines[:n]
	return strings.Join(kept, "\n") + fmt.Sprintf("\n... (max %d lines; narrow path/glob or raise max_results)", n)
}

// searchPerFileCap maps a global max_results into a per-file rg --max-count.
func searchPerFileCap(global int) int {
	if global <= 0 {
		global = defaultSearchGlobal
	}
	if global < defaultSearchPerFile {
		return global
	}
	return defaultSearchPerFile
}

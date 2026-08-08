package server

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

// Server-side hard ceilings for client-supplied max/max_results arguments.
// Pi/other hosts clamp on their side, but direct MCP clients bypass that, so
// the server must bound every limit itself: an unbounded limit means an
// unbounded SQL LIMIT, an unbounded rg buffer, or an unbounded JSON payload
// (memory/CPU amplification, audit H2). Values are conservative — far above
// any normal usage (a single tool result is truncated to defaultOutputChars
// anyway) but small enough that a hostile request cannot drive the daemon's
// RSS.
const (
	// search: global match cap (per-file cap is defaultSearchPerFile).
	maxSearchResults = 500

	// files: path listing cap.
	maxFilesResults = 2000

	// callers / callees / impact: symbol result cap.
	maxSymbolResults = 200

	// search_facts: result row cap.
	maxFactsResults = 100

	// communities: reported community cap (Louvain output).
	maxCommunities = 100

	// store_fact: maximum fact content bytes accepted for insert.
	maxFactContentLen = 64 * 1024

	// Facts echoed back in store_fact/search_facts responses: at most this
	// many rows per target, and each row's content is truncated to
	// maxFactContentShown bytes before marshaling (read-back LIMIT + cap).
	maxFactsReadback    = 50
	maxFactContentShown = 2000

	// rg streaming: stop reading stdout after this many total bytes
	// (line cap is per-action). Bounds the intermediate buffer before
	// truncateOutput cuts the final payload.
	rgMaxOutputBytes = 512 * 1024
)

// clampLimit normalizes a client-supplied limit: <=0 falls back to the
// action default, values above the hard ceiling are cut to the ceiling.
// Every action routes its max through this before use, so a negative or
// absurdly large argument can never panic (negative slice index) or amplify
// memory/CPU (audit critical #2, high H2).
func clampLimit(v, def, hard int) int {
	if v <= 0 {
		return def
	}
	if v > hard {
		return hard
	}
	return v
}

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

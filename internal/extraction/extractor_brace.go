package extraction

import (
	"strings"
)

// maxBraceScanLines caps findBraceEnd's forward scan: an unterminated '{'
// must not force a scan to EOF for every symbol of a pathological file
// (CPU amplification). Past the budget the declaration line alone is
// returned, like an unterminated block.
const maxBraceScanLines = 20000

// braceScanner is the shared line scanner behind findBraceEnd and
// extractJSClassMethods: it tracks brace depth while ignoring braces inside
// strings, comments and JS regex literals.

// braceScanner is the shared line scanner behind findBraceEnd and
// extractJSClassMethods: it tracks brace depth while ignoring braces inside
// strings, comments and JS regex literals.
type braceScanner struct {
	depth          int
	inString       bool
	stringChar     byte
	triple         bool // Python """ / ''' string
	inLineComment  bool
	inBlockComment bool
}

// scan advances the scanner over one line and reports whether the brace
// depth dropped to zero inside it (i.e. the tracked block closed).

// scan advances the scanner over one line and reports whether the brace
// depth dropped to zero inside it (i.e. the tracked block closed).
func (s *braceScanner) scan(line string) bool {
	s.inLineComment = false // comment state is per-line
	for j := 0; j < len(line); j++ {
		ch := line[j]
		if s.inLineComment {
			break // rest of the line is comment
		}
		if s.inBlockComment {
			if ch == '*' && j+1 < len(line) && line[j+1] == '/' {
				s.inBlockComment = false
				j++
			}
			continue
		}
		if s.inString {
			if s.triple {
				if ch == s.stringChar && j+2 < len(line) && line[j+1] == s.stringChar && line[j+2] == s.stringChar {
					s.inString = false
					s.triple = false
					j += 2
				}
				continue
			}
			// Go raw strings (backticks) do not treat '\' as an escape:
			// skipping the next char could swallow the closing backtick and
			// leave the string open until EOF (must-fix).
			if ch == '\\' && j+1 < len(line) && s.stringChar != '`' {
				j++ // skip escaped char
				continue
			}
			if ch == s.stringChar {
				s.inString = false
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			// Python triple-quoted strings: braces inside """...""" must
			// not count as block braces (must-fix).
			if j+2 < len(line) && line[j+1] == ch && line[j+2] == ch {
				s.inString = true
				s.stringChar = ch
				s.triple = true
				j += 2
				continue
			}
			s.inString = true
			s.stringChar = ch
			s.triple = false
			continue
		}
		if ch == '`' {
			s.inString = true
			s.stringChar = ch
			s.triple = false
			continue
		}
		if ch == '/' && j+1 < len(line) {
			if line[j+1] == '/' {
				s.inLineComment = true
				j++
				continue
			}
			if line[j+1] == '*' {
				s.inBlockComment = true
				j++
				continue
			}
			// JS regex literal: braces inside /.../ are not block braces
			// (must-fix). Division '/' is preceded by an operand and is not
			// treated as a regex start.
			if isRegexStart(line, j) {
				j = scanRegexLiteral(line, j)
				continue
			}
		}
		if ch == '{' {
			s.depth++
		} else if ch == '}' && s.depth > 0 {
			s.depth--
			if s.depth == 0 {
				return true
			}
		}
	}
	return false
}

// isRegexStart reports whether the '/' at line[j] starts a JS regex literal
// rather than a division. Heuristic: a '/' directly after an operand
// (identifier, number, closing bracket/quote, or a ++/-- prefix) is division;
// after operators, openers or at line start it is a regex literal.

// isRegexStart reports whether the '/' at line[j] starts a JS regex literal
// rather than a division. Heuristic: a '/' directly after an operand
// (identifier, number, closing bracket/quote, or a ++/-- prefix) is division;
// after operators, openers or at line start it is a regex literal.
func isRegexStart(line string, j int) bool {
	k := j - 1
	for k >= 0 && (line[k] == ' ' || line[k] == '\t') {
		k--
	}
	if k < 0 {
		return true // start of line: regex
	}
	prev := line[k]
	switch {
	case prev >= 'a' && prev <= 'z', prev >= 'A' && prev <= 'Z', prev >= '0' && prev <= '9',
		prev == '_', prev == '$', prev == ')', prev == ']', prev == '}',
		prev == '\'', prev == '"', prev == '`':
		return false // after an operand: division
	case prev == '+' || prev == '-':
		// a++ / b is division; + /re/ after an operator is a regex.
		k2 := k - 1
		for k2 >= 0 && (line[k2] == ' ' || line[k2] == '\t') {
			k2--
		}
		if k2 >= 0 && (line[k2] == '+' || line[k2] == '-') {
			return false // ++ / -- prefix: operand, so division
		}
		return true
	}
	return true
}

// scanRegexLiteral scans a JS regex literal starting at the '/' in line[j]
// and returns the index of its closing '/'. Escapes and character classes
// are honored; an unterminated regex consumes the rest of the line.

// scanRegexLiteral scans a JS regex literal starting at the '/' in line[j]
// and returns the index of its closing '/'. Escapes and character classes
// are honored; an unterminated regex consumes the rest of the line.
func scanRegexLiteral(line string, j int) int {
	inClass := false
	for j++; j < len(line); j++ {
		c := line[j]
		if c == '\\' {
			j++
			continue
		}
		if c == '[' {
			inClass = true
			continue
		}
		if c == ']' {
			inClass = false
			continue
		}
		if c == '/' && !inClass {
			return j
		}
	}
	return j // unterminated: consume to end of line
}

// findBraceEnd finds the line where the matching closing brace is (returned
// as the 1-based line of the closing brace, i.e. an exclusive end index).
// Braces inside strings, char literals, line comments (//), block comments
// (/* */) and JS regex literals are ignored, so a comment/string/regex
// containing '{' or '}' can no longer truncate or stretch a symbol's range.
// The forward scan is capped at maxBraceScanLines: an unterminated block
// returns start+1 (the declaration line only) instead of scanning a
// pathological file to EOF.

// findBraceEnd finds the line where the matching closing brace is (returned
// as the 1-based line of the closing brace, i.e. an exclusive end index).
// Braces inside strings, char literals, line comments (//), block comments
// (/* */) and JS regex literals are ignored, so a comment/string/regex
// containing '{' or '}' can no longer truncate or stretch a symbol's range.
// The forward scan is capped at maxBraceScanLines: an unterminated block
// returns start+1 (the declaration line only) instead of scanning a
// pathological file to EOF.
func findBraceEnd(lines []string, start int) int {
	sc := braceScanner{}
	limit := start + maxBraceScanLines
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := start; i < limit; i++ {
		if sc.scan(lines[i]) {
			return i + 1
		}
	}
	return start + 1
}

// findIndentEnd finds the end of an indented block (Python). Blank lines and
// comment-only lines after the header are skipped when measuring the block's
// base indent, so `def f():` followed by an empty line (or a docstring/comment
// line) no longer collapses the body to a single line.

// findIndentEnd finds the end of an indented block (Python). Blank lines and
// comment-only lines after the header are skipped when measuring the block's
// base indent, so `def f():` followed by an empty line (or a docstring/comment
// line) no longer collapses the body to a single line.
func findIndentEnd(lines []string, start int) int {
	if start+1 >= len(lines) {
		return start + 1
	}

	baseIndent := -1
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		baseIndent = countIndent(lines[i])
		if baseIndent == 0 {
			return start + 1 // next statement is dedented — empty body
		}
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if countIndent(lines[j]) < baseIndent {
				return j
			}
		}
		return len(lines)
	}
	return start + 1
}

func countIndent(line string) int {
	count := 0
	for _, ch := range line {
		if ch == ' ' || ch == '\t' {
			count++
		} else {
			break
		}
	}
	return count
}

func extractBody(lines []string, start, end int) string {
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

// isGoKeyword is ONLY real language keywords (not builtins). Builtins like
// close/new/make may be user-defined; those calls must stay extractable.

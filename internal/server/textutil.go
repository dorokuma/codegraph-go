package server

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// isSimpleIdent reports whether s looks like a bare symbol name (no regex).
func isSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '$':
			continue
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isWordIn reports whether word appears as a standalone word in text.
// Word boundaries are: start/end of string, space, slash, dot, dash, underscore.
func isWordIn(word, text string) bool {
	idx := strings.Index(text, word)
	if idx < 0 {
		return false
	}
	end := idx + len(word)
	leftOK := idx == 0 || isWordSep(text[idx-1])
	rightOK := end == len(text) || isWordSep(text[end])
	return leftOK && rightOK
}

func isWordSep(b byte) bool {
	switch b {
	case ' ', '/', '.', '-', '_', ',', ':', '\t', '\n', '(', ')', '[', ']', '{', '}':
		return true
	}
	return false
}

// getCachedDefRe returns a compiled regex that matches definitions of the given name.
// The result is cached per name to avoid repeated MustCompile across invocations.
func (s *Server) getCachedDefRe(name string) *regexp.Regexp {
	if cached, ok := s.DefReCache.Load(name); ok {
		return cached.(*regexp.Regexp)
	}
	quoted := regexp.QuoteMeta(name)
	re := regexp.MustCompile(`(func\s+(\([^)]*\)\s*)?|def\s+|function\s+|class\s+|fn\s+)` + quoted + `\b`)
	s.DefReCache.Store(name, re)
	return re
}

// relativizeRgOutput converts absolute file paths in rg output to paths relative
// to projRoot, so they match the format used by FTS/explore/node tools.
func relativizeRgOutput(out string, projRoot string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		rel := db.RelPath(projRoot, parts[0])
		fmt.Fprintf(&b, "%s:%s\n", rel, parts[1])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// countIndexedUnder returns the number of indexed files whose path is under the given root.
func countIndexedUnder(ctx context.Context, database *db.DB, projRoot, searchRoot string) (int, error) {
	return database.CountFilesUnderContext(ctx, searchRoot)
}

func readLines(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 10*1024*1024 {
		return nil, fmt.Errorf("file %q is too large (> 10MB)", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	return strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), nil
}

func stripStringsAndComments(line string) string {
	var out strings.Builder
	out.Grow(len(line))
	inString := false
	stringChar := byte(0)
	skip := false
	for j := 0; j < len(line); j++ {
		ch := line[j]
		if skip {
			skip = false
			out.WriteByte(' ')
			continue
		}
		if !inString && ch == '/' && j+1 < len(line) {
			if line[j+1] == '/' {
				for ; j < len(line); j++ {
					out.WriteByte(' ')
				}
				break
			}
			if line[j+1] == '*' {
				out.WriteByte(' ')
				out.WriteByte(' ')
				j++
				for j+1 < len(line) {
					if line[j] == '*' && line[j+1] == '/' {
						out.WriteByte(' ')
						out.WriteByte(' ')
						j++
						break
					}
					out.WriteByte(' ')
					j++
				}
				continue
			}
		}
		if inString {
			if stringChar == 0 {
				if ch == '*' && j+1 < len(line) && line[j+1] == '/' {
					out.WriteByte(' ')
					out.WriteByte(' ')
					j++
					inString = false
				} else {
					out.WriteByte(' ')
				}
				continue
			}
			if ch == '\\' {
				out.WriteByte(' ')
				if stringChar != '`' {
					skip = true
					continue
				}
			}
			if ch == stringChar {
				inString = false
			}
			out.WriteByte(' ')
			continue
		}
		if ch == '"' || ch == '\'' || ch == '`' {
			if (ch == '\'' || ch == '"') && j+2 < len(line) && line[j+1] == ch && line[j+2] == ch {
				for ; j < len(line); j++ {
					out.WriteByte(' ')
				}
				break
			}
			inString = true
			stringChar = ch
			out.WriteByte(' ')
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}

func countLeadingSpaces(line string) int {
	n := 0
	for _, ch := range line {
		if ch == ' ' || ch == '\t' {
			n++
		} else {
			break
		}
	}
	return n
}

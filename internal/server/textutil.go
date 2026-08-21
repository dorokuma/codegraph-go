package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// matchLineInNode returns the file line of the first pattern occurrence in the
// indexed body. Name-only FTS hits keep the symbol start line.
func matchLineInNode(n db.Node, pattern string) int {
	if n.Line <= 0 {
		n.Line = 1
	}
	if pattern == "" || n.Body == "" {
		return n.Line
	}
	idx := strings.Index(n.Body, pattern)
	if idx < 0 {
		return n.Line
	}
	return n.Line + strings.Count(n.Body[:idx], "\n")
}

// matchLineForNode returns the file line of the first pattern occurrence in n.
// When n already has a body, it inspects n.Body.
// When n has no body (lightweight ref), if n.Name matches pattern or n.Line is given,
// it reuses n.Line; otherwise it checks the file on disk in the symbol range.
func matchLineForNode(projRoot string, n db.Node, pattern string) int {
	if n.Line <= 0 {
		n.Line = 1
	}
	if pattern == "" || n.Name == pattern {
		return n.Line
	}
	if n.Body != "" {
		idx := strings.Index(n.Body, pattern)
		if idx >= 0 {
			return n.Line + strings.Count(n.Body[:idx], "\n")
		}
		return n.Line
	}
	filePath := db.AbsPath(projRoot, n.File)
	lines, err := readLines(filePath)
	if err != nil || len(lines) == 0 {
		return n.Line
	}
	start := n.Line - 1
	if start < 0 {
		start = 0
	}
	end := n.EndLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if end < start {
		end = len(lines)
	}
	for i := start; i < end && i < len(lines); i++ {
		if strings.Contains(lines[i], pattern) {
			return i + 1
		}
	}
	for i, l := range lines {
		if strings.Contains(l, pattern) {
			return i + 1
		}
	}
	return n.Line
}

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

// defReCacheCap bounds the compiled-definition-regex cache (M6). One entry
// per distinct queried symbol name: unbounded, a long-lived daemon would leak
// a compiled regex per symbol it ever saw.
const defReCacheCap = 256

// defReCache is a bounded FIFO cache for compiled definition regexes (M6).
// The zero value is usable; get compiles on miss and evicts the oldest entry
// when the cap is exceeded. Concurrent-safe: MCP tool handlers may call it in
// parallel.
type defReCache struct {
	mu    sync.Mutex
	m     map[string]*regexp.Regexp
	order []string // insertion order, oldest first
	cap   int
}

// get returns the cached regex for name, compiling and caching it on miss.
func (c *defReCache) get(name string) *regexp.Regexp {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]*regexp.Regexp)
		c.cap = defReCacheCap
	}
	if re, ok := c.m[name]; ok {
		return re
	}
	quoted := regexp.QuoteMeta(name)
	re := regexp.MustCompile(`(func\s+(\([^)]*\)\s*)?|def\s+|function\s+|class\s+|fn\s+)` + quoted + `\b`)
	c.m[name] = re
	c.order = append(c.order, name)
	if len(c.order) > c.cap {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	return re
}

// getCachedDefRe returns a compiled regex that matches definitions of the given name.
// The result is cached per name (bounded FIFO, M6) to avoid repeated MustCompile.
func (s *Server) getCachedDefRe(name string) *regexp.Regexp {
	return s.DefReCache.get(name)
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

// rgOutputLines runs cmd (an rg invocation) with stdout piped and returns up
// to maxLines lines / maxBytes bytes of output, reading streamingly so a
// huge rg result never lands in memory whole (audit high: rg.Output() loads
// everything before limitLines cuts it). Reading stops as soon as either cap
// is hit and the process is killed; a killed process is reported as
// truncated, not as an error.
//
// rg's exit code 1 (no matches) is not an error: it returns whatever lines
// were read and a nil error. Any other exit (spawn failure, rg error, e.g.
// a bad pattern on exit 2) is returned as err, matching the semantics the
// callers previously got from Cmd.Output.
//
// Cleanup: after Start, every return path reaps the child via the deferred
// Kill+Wait — a panic inside the read loop can no longer leave a zombie.
// Kill runs before Wait: waiting on a child that is still writing into a
// full stdout pipe would otherwise hang past the caller's ctx deadline
// (CommandContext only kills on ctx cancel; a plain read error has no such
// backstop).
// rgEachRoot runs rg once per root and merges lines, stopping at the same
// line/byte caps as a single rgOutputLines call.
func rgEachRoot(roots []string, maxLines, maxBytes int, build func(root string) *exec.Cmd) ([]string, error) {
	var all []string
	var firstErr error
	remainLines := maxLines
	remainBytes := maxBytes
	for _, root := range roots {
		if remainLines <= 0 || remainBytes <= 0 {
			break
		}
		lines, _, err := rgOutputLines(build(root), remainLines, remainBytes)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		all = append(all, lines...)
		remainLines -= len(lines)
		for _, l := range lines {
			remainBytes -= len(l) + 1
		}
	}
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return all, nil
}

func rgOutputLines(cmd *exec.Cmd, maxLines, maxBytes int) (lines []string, truncated bool, err error) {
	if maxLines <= 0 {
		maxLines = 1 << 30
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 30
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	defer func() {
		// Kill first: a child still blocked writing into a full pipe would
		// make Wait hang until the caller's ctx kills it (or forever on a
		// ctx-less call). Wait reaps, so no zombie is left even if the read
		// loop above panicked. Both calls are no-ops when the child already
		// exited and was waited on — errors are ignored.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	br := bufio.NewReader(stdout)
	var total int
	for len(lines) < maxLines && total < maxBytes {
		var line []byte
		for {
			frag, ferr := br.ReadSlice('\n')
			line = append(line, frag...)
			if len(line) > maxBytes {
				// Pathological single line: over budget, stop entirely.
				total += len(line)
				truncated = true
				break
			}
			if ferr == nil {
				break
			}
			if ferr == bufio.ErrBufferFull {
				continue // line longer than the read buffer; keep reading fragments
			}
			if len(line) > 0 {
				s := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
				lines = append(lines, s)
			}
			if ferr == io.EOF {
				goto done
			}
			// Read error (e.g. killed by context): kill the child first so
			// Wait below cannot hang on a still-writing process, then surface
			// the exit status.
			_ = cmd.Process.Kill()
			goto wait
		}
		if truncated {
			break
		}
		total += len(line)
		if len(line) > 0 {
			s := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			lines = append(lines, s)
		}
	}
	if len(lines) >= maxLines || total >= maxBytes {
		truncated = true
	}
done:
	if truncated {
		// We stopped early on purpose (line/byte cap): the deferred cleanup
		// kills the child so it cannot keep writing into the pipe and reaps
		// it; the exit status of a killed child is irrelevant here.
		return lines, true, nil
	}
wait:
	werr := cmd.Wait()
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			// rg's "no matches" exit: not an error.
			return lines, false, nil
		}
		return lines, false, werr
	}
	return lines, false, nil
}

// countIndexedUnder returns the number of indexed files whose path is under the given root.
// searchRoot is absolute on disk; the index stores workdir-relative keys.
func countIndexedUnder(ctx context.Context, database *db.DB, projRoot, searchRoot string) (int, error) {
	return database.CountFilesUnderContext(ctx, db.StoragePath(projRoot, searchRoot))
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

package resolution

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// C/C++ function-pointer dispatch (official c-fnptr-synthesizer subset).
//
// Patterns covered (high precision):
//  1. Registration:  .field = handler   /  ptr->field = handler
//  2. Designated init: { .field = handler, ... }
//  3. Dispatch:      recv->field(...)  /  recv.field(...)
//
// Edge: enclosing function of dispatch → registered handler
// metadata.synthesizedBy = "fn-pointer-dispatch"

const fnptrFanoutCap = 40

var (
	// .run = add   or  ->run = add  (identifier RHS only)
	fnptrAssignRe = regexp.MustCompile(`(?:->|\.)\s*([A-Za-z_]\w*)\s*=\s*&?\s*([A-Za-z_]\w*)\b`)
	// dispatch: foo->run(  or  foo.run(  or  (foo->run)(
	fnptrDispatchRe = regexp.MustCompile(`(?:(?:\w+\s*(?:\[[^\]]*\])?\s*(?:->|\.)\s*)+)([A-Za-z_]\w*)\s*\)?\s*\(`)
)

type fnptrHit struct {
	id   int64
	name string
	file string
	line int
}

// cFnPointerDispatchEdges synthesizes dispatcher→handler calls for C/C++ fn-pointers.
func cFnPointerDispatchEdges(ctx *synthCtx) ([]synthEdge, error) {
	regs := map[string][]fnptrHit{} // field → handlers

	for _, file := range ctx.allFiles {
		lang := langOfPath(file)
		if lang != "c" && lang != "cpp" {
			continue
		}
		src := ctx.readFile(file)
		if src == "" {
			continue
		}
		safe := stripCComments(src)
		funcsInFile := callableByName(ctx.getNodesInFile(file))

		for _, m := range fnptrAssignRe.FindAllStringSubmatchIndex(safe, -1) {
			if len(m) < 6 {
				continue
			}
			field := safe[m[2]:m[3]]
			handler := safe[m[4]:m[5]]
			if field == "" || handler == "" || field == handler {
				continue
			}
			hn := funcsInFile[handler]
			if hn == nil {
				hn = firstCallable(ctx.getNodesByName(handler))
			}
			if hn == nil {
				continue
			}
			line := lineAt(safe, m[0])
			regs[field] = append(regs[field], fnptrHit{id: hn.ID, name: hn.Name, file: file, line: line})
		}
	}
	if len(regs) == 0 {
		return nil, nil
	}

	for f, list := range regs {
		seen := map[int64]bool{}
		var uniq []fnptrHit
		for _, h := range list {
			if seen[h.id] {
				continue
			}
			seen[h.id] = true
			uniq = append(uniq, h)
		}
		regs[f] = uniq
	}

	var edges []synthEdge
	seenEdge := map[string]bool{}
	added := 0

	for _, file := range ctx.allFiles {
		lang := langOfPath(file)
		if lang != "c" && lang != "cpp" {
			continue
		}
		src := ctx.readFile(file)
		if src == "" {
			continue
		}
		safe := stripCComments(src)
		fileNodes := ctx.getNodesInFile(file)

		for _, m := range fnptrDispatchRe.FindAllStringSubmatchIndex(safe, -1) {
			if len(m) < 4 {
				continue
			}
			field := safe[m[2]:m[3]]
			handlers := regs[field]
			if len(handlers) == 0 {
				continue
			}
			full := safe[m[0]:m[1]]
			if !strings.Contains(full, "->") && !strings.Contains(full, ".") {
				continue
			}
			line := lineAt(safe, m[0])
			enc := enclosingFn(fileNodes, line)
			if enc == nil {
				continue
			}
			n := 0
			for _, h := range handlers {
				if h.id == enc.ID {
					continue
				}
				key := edgeKey(enc.ID, h.id, db.EdgeCalls)
				if seenEdge[key] {
					continue
				}
				seenEdge[key] = true
				edges = append(edges, synthEdge{
					SourceID: enc.ID,
					TargetID: h.id,
					Kind:     db.EdgeCalls,
					File:     file,
					Line:     line,
					Meta: map[string]string{
						"synthesizedBy": "fn-pointer-dispatch",
						"via":           field,
						"handler":       h.name,
					},
				})
				added++
				n++
				if n >= fnptrFanoutCap || added >= fnptrFanoutCap*20 {
					break
				}
			}
			if added >= fnptrFanoutCap*20 {
				return edges, nil
			}
		}
	}
	return edges, nil
}

func langOfPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	default:
		return ""
	}
}

func callableByName(nodes []db.Node) map[string]*db.Node {
	out := map[string]*db.Node{}
	for i := range nodes {
		n := &nodes[i]
		if isCallableKind(n.Kind) {
			out[n.Name] = n
		}
	}
	return out
}

func firstCallable(nodes []db.Node) *db.Node {
	for i := range nodes {
		if isCallableKind(nodes[i].Kind) {
			return &nodes[i]
		}
	}
	return nil
}

func stripCComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// String literal: skip "..." (including escaped \")
		if s[i] == '"' {
			b.WriteByte(s[i])
			i++
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					b.WriteByte(s[i])
					i++
					b.WriteByte(s[i])
					i++
				} else if s[i] == '"' {
					b.WriteByte(s[i])
					i++
					break
				} else {
					b.WriteByte(s[i])
					i++
				}
			}
			continue
		}
		// Char literal: skip '...' (including escaped \')
		if s[i] == '\'' {
			b.WriteByte(s[i])
			i++
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					b.WriteByte(s[i])
					i++
					b.WriteByte(s[i])
					i++
				} else if s[i] == '\'' {
					b.WriteByte(s[i])
					i++
					break
				} else {
					b.WriteByte(s[i])
					i++
				}
			}
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '/' {
			b.WriteByte(' ')
			b.WriteByte(' ')
			i += 2
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			b.WriteByte(' ')
			b.WriteByte(' ')
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				if s[i] == '\n' {
					b.WriteByte('\n')
				} else {
					b.WriteByte(' ')
				}
				i++
			}
			if i+1 < len(s) {
				b.WriteByte(' ')
				b.WriteByte(' ')
				i += 2
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

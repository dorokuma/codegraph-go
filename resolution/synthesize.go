package resolution

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/dorokuma/codegraph-go/db"
)

// SynthStats summarizes one SynthesizeAll pass.
type SynthStats struct {
	Written int
	ByPass  map[string]int
}

const (
	maxCallbacksPerChannel = 40
	eventFanoutCap         = 6
	maxJSXChildren         = 30
)

var (
	registrarNameRe  = regexp.MustCompile(`^(on[A-Z]\w*|subscribe|addListener|addEventListener|register|watch|listen|addCallback)$`)
	dispatcherNameRe = regexp.MustCompile(`(?i)(emit|trigger|notify|dispatch|fire|publish|flush)`)
	registrarFieldRe = regexp.MustCompile(`this\.(\w+)\.(?:add|push|set)\(`)
	dispatcherForOf  = regexp.MustCompile(`\bof\s+(?:Array\.from\(\s*)?this\.(\w+)`)
	dispatcherForEach = regexp.MustCompile(`this\.(\w+)\.forEach\(`)
	onEventRe        = regexp.MustCompile(`\.(?:on|once|addListener)\(\s*['"]([^'"]+)['"]\s*,\s*(?:function\s+(\w+)|(?:this\.)?(\w+))`)
	emitEventRe      = regexp.MustCompile(`\.(?:emit|fire|dispatchEvent)\(\s*['"]([^'"]+)['"]`)
	setStateRe       = regexp.MustCompile(`this\.setState\s*\(`)
	jsxTagRe         = regexp.MustCompile(`<([A-Z][A-Za-z0-9_]*)[\s/>]`)
	wordParenRe      = regexp.MustCompile(`\b\w+\s*\(`)
)

// SynthesizeAll runs whole-graph dynamic-dispatch synthesis after base resolution.
// Edges are provenance=heuristic with metadata.synthesizedBy (official-aligned).
func SynthesizeAll(database *db.DB, workdir string) (SynthStats, error) {
	st := SynthStats{ByPass: map[string]int{}}
	if err := database.DeleteSynthesizedEdges(); err != nil {
		return st, err
	}

	ctx := newSynthCtx(database, workdir)

	type pass struct {
		name string
		run  func(*synthCtx) ([]synthEdge, error)
	}
	passes := []pass{
		{"callback", fieldChannelEdges},
		{"event-emitter", eventEmitterEdges},
		{"react-render", reactRenderEdges},
		{"jsx-render", reactJSXChildEdges},
		{"bridge-link", bridgeSymbolEdges},
		// 7.5 long-tail
		{"fn-pointer-dispatch", cFnPointerDispatchEdges},
		{"goframe-route", goframeRouteEdges},
	}

	seen := map[string]bool{}
	var merged []synthEdge
	for _, p := range passes {
		edges, err := p.run(ctx)
		if err != nil {
			log.Printf("synthesis %s: %v", p.name, err)
			continue
		}
		n := 0
		for _, e := range edges {
			key := edgeKey(e.SourceID, e.TargetID, e.Kind)
			if seen[key] || e.SourceID == 0 || e.TargetID == 0 || e.SourceID == e.TargetID {
				continue
			}
			seen[key] = true
			merged = append(merged, e)
			n++
		}
		st.ByPass[p.name] = n
	}

	for _, e := range merged {
		meta, _ := json.Marshal(e.Meta)
		if _, err := database.UpsertEdge(&db.Edge{
			SourceID:   e.SourceID,
			TargetID:   e.TargetID,
			Kind:       e.Kind,
			File:       e.File,
			Line:       e.Line,
			Provenance: ProvHeuristic,
			Metadata:   string(meta),
		}); err != nil {
			log.Printf("synthesis upsert edge %d->%d [%s] %s:%d: %v", e.SourceID, e.TargetID, e.Kind, e.File, e.Line, err)
			continue
		}
		st.Written++
	}
	return st, nil
}

func edgeKey(src, tgt int64, kind string) string {
	return strings.Join([]string{
		itoa(src), ">", itoa(tgt), ":", kind,
	}, "")
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}


type synthEdge struct {
	SourceID int64
	TargetID int64
	Kind     string
	File     string
	Line     int
	Meta     map[string]string
}

// lruCache is a bounded in-memory cache with LRU eviction.
type lruCache struct {
	max   int
	keys  []string
	store map[string]string
}

func newLRUCache(max int) *lruCache {
	return &lruCache{
		max:   max,
		keys:  make([]string, 0, max),
		store: make(map[string]string),
	}
}

func (l *lruCache) get(key string) (string, bool) {
	v, ok := l.store[key]
	if ok {
		l.touch(key)
	}
	return v, ok
}

func (l *lruCache) put(key, value string) {
	if _, ok := l.store[key]; ok {
		l.store[key] = value
		l.touch(key)
		return
	}
	if len(l.keys) >= l.max {
		oldest := l.keys[len(l.keys)-1]
		l.keys = l.keys[:len(l.keys)-1]
		delete(l.store, oldest)
	}
	l.store[key] = value
	l.keys = append([]string{key}, l.keys...)
}

func (l *lruCache) touch(key string) {
	for i, k := range l.keys {
		if k == key {
			l.keys = append(l.keys[:i], l.keys[i+1:]...)
			break
		}
	}
	l.keys = append([]string{key}, l.keys...)
}

func (l *lruCache) len() int { return len(l.store) }

const fileContentMaxEntries = 64

type synthCtx struct {
	db      *db.DB
	workdir string
	// caches — avoid repeated full-table GetNodesByKind scans (memory/CPU).
	fileContent *lruCache
	nodesByName map[string][]db.Node
	nodesInFile map[string][]db.Node
	nodesByKind map[string][]db.Node
	allFiles    []string
}

func newSynthCtx(database *db.DB, workdir string) *synthCtx {
	ctx := &synthCtx{
		db:          database,
		workdir:     workdir,
		fileContent: newLRUCache(fileContentMaxEntries),
		nodesByName: map[string][]db.Node{},
		nodesInFile: map[string][]db.Node{},
		nodesByKind: map[string][]db.Node{},
	}
	files, _ := database.ListFiles()
	ctx.allFiles = files
	return ctx
}

// nodesOfKind returns all nodes of kind, cached for the synthesis pass.
func (c *synthCtx) nodesOfKind(kind string) ([]db.Node, error) {
	if v, ok := c.nodesByKind[kind]; ok {
		return v, nil
	}
	nodes, err := c.db.GetNodesByKind(kind)
	if err != nil {
		return nil, err
	}
	c.nodesByKind[kind] = nodes
	return nodes, nil
}

func (c *synthCtx) readFile(path string) string {
	if path == "" {
		return ""
	}
	if s, ok := c.fileContent.get(path); ok {
		return s
	}
	// path may be absolute already
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.workdir, path)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		// try as-is
		data, err = os.ReadFile(path)
		if err != nil {
			c.fileContent.put(path, "")
			return ""
		}
	}
	s := string(data)
	c.fileContent.put(path, s)
	return s
}

func (c *synthCtx) getNodesByName(name string) []db.Node {
	if v, ok := c.nodesByName[name]; ok {
		return v
	}
	nodes, err := c.db.GetNodeByName(name)
	if err != nil {
		nodes = nil
	}
	c.nodesByName[name] = nodes
	return nodes
}

func (c *synthCtx) getNodesInFile(file string) []db.Node {
	if v, ok := c.nodesInFile[file]; ok {
		return v
	}
	nodes, err := c.db.GetNodesByFile(file)
	if err != nil {
		nodes = nil
	}
	c.nodesInFile[file] = nodes
	return nodes
}

func sliceLines(content string, start, end int) string {
	if content == "" || start <= 0 {
		return ""
	}
	lines := strings.Split(content, "\n")
	if start > len(lines) {
		return ""
	}
	if end < start {
		end = start
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n")
}

func lineAt(content string, byteIdx int) int {
	if byteIdx <= 0 {
		return 1
	}
	if byteIdx > len(content) {
		byteIdx = len(content)
	}
	return strings.Count(content[:byteIdx], "\n") + 1
}

func isCallableKind(kind string) bool {
	switch kind {
	case db.KindFunction, db.KindMethod, "component", "constructor":
		return true
	default:
		return false
	}
}

func enclosingFn(nodes []db.Node, line int) *db.Node {
	var best *db.Node
	for i := range nodes {
		n := &nodes[i]
		if !isCallableKind(n.Kind) {
			continue
		}
		end := n.EndLine
		if end == 0 {
			end = n.Line
		}
		if n.Line <= line && line <= end {
			if best == nil || n.Line >= best.Line {
				best = n
			}
		}
	}
	return best
}

func registrarField(src string) string {
	m := registrarFieldRe.FindStringSubmatch(src)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func dispatcherField(src string) string {
	if m := dispatcherForOf.FindStringSubmatch(src); len(m) > 1 {
		if wordParenRe.MatchString(src) {
			return m[1]
		}
	}
	if m := dispatcherForEach.FindStringSubmatch(src); len(m) > 1 {
		return m[1]
	}
	return ""
}

// resolveCallbackTarget picks the best callable node named cbName in the context
// of a registration call made at caller. It prefers same-file, same-directory,
// and same-type (via class-method containment) over a blind global first match.
func resolveCallbackTarget(ctx *synthCtx, cbName string, caller *db.Node, reg db.Node) *db.Node {
	nodes := ctx.getNodesByName(cbName)
	var callables []db.Node
	for _, n := range nodes {
		if n.Kind == db.KindMethod || n.Kind == db.KindFunction {
			callables = append(callables, n)
		}
	}
	if len(callables) == 0 {
		return nil
	}
	if len(callables) == 1 {
		n := callables[0]
		return &n
	}

	// Score each candidate: lower is better.
	type cand struct {
		n     db.Node
		score int
	}
	var ranked []cand
	callerDir := filepath.Dir(caller.File)
	regDir := filepath.Dir(reg.File)
	for _, c := range callables {
		sc := 100
		cDir := filepath.Dir(c.File)
		if c.File == caller.File {
			sc = 1
		} else if cDir == callerDir {
			sc = 2
		} else if c.File == reg.File {
			sc = 3
		} else if cDir == regDir {
			sc = 4
		} else if strings.HasPrefix(c.File, callerDir+string(filepath.Separator)) {
			sc = 10
		}
		ranked = append(ranked, cand{n: c, score: sc})
	}

	// Stable: pick the lowest score; if tie, first in original order wins (fallback).
	best := &ranked[0]
	for i := 1; i < len(ranked); i++ {
		if ranked[i].score < best.score {
			best = &ranked[i]
		}
	}
	n := best.n
	return &n
}

// argReCache avoids recompiling the registrar-name regex per call site.
var argReCache sync.Map

// getArgRe returns a compiled regex that matches calls to name, e.g.
//   name( this.callback )
// Cached by name so repeated hits on the same registrar don't recompile.
func getArgRe(name string) *regexp.Regexp {
	if v, ok := argReCache.Load(name); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `\s*\(\s*(?:this\.)?(\w+)`)
	actual, _ := argReCache.LoadOrStore(name, re)
	return actual.(*regexp.Regexp)
}

// fieldChannelEdges: registrar/dispatcher share a field store → dispatcher calls registered callbacks.
func fieldChannelEdges(ctx *synthCtx) ([]synthEdge, error) {
	methods, err := ctx.nodesOfKind(db.KindMethod)
	if err != nil {
		return nil, err
	}
	funcs, err := ctx.nodesOfKind(db.KindFunction)
	if err != nil {
		return nil, err
	}
	candidates := append(methods, funcs...)

	type pair struct {
		node  db.Node
		field string
	}
	var registrars, dispatchers []pair
	for _, m := range candidates {
		isReg := registrarNameRe.MatchString(m.Name)
		isDisp := dispatcherNameRe.MatchString(m.Name)
		if !isReg && !isDisp {
			continue
		}
		content := ctx.readFile(m.File)
		src := sliceLines(content, m.Line, m.EndLine)
		if src == "" {
			// fall back to stored body
			src = m.Body
		}
		if src == "" {
			continue
		}
		if isReg {
			if f := registrarField(src); f != "" {
				registrars = append(registrars, pair{node: m, field: f})
			}
		}
		if isDisp {
			if f := dispatcherField(src); f != "" {
				dispatchers = append(dispatchers, pair{node: m, field: f})
			}
		}
	}

	var edges []synthEdge
	seen := map[string]bool{}
	for _, reg := range registrars {
		var chDisp []pair
		for _, d := range dispatchers {
			if d.node.File == reg.node.File && d.field == reg.field {
				chDisp = append(chDisp, d)
			}
		}
		if len(chDisp) == 0 {
			continue
		}
		argRe := getArgRe(reg.node.Name)
		incoming, err := ctx.db.GetIncomingEdges(reg.node.ID, []string{db.EdgeCalls})
		if err != nil {
			continue
		}
		added := 0
		for _, e := range incoming {
			if added >= maxCallbacksPerChannel {
				break
			}
			if e.Line <= 0 {
				continue
			}
			caller, err := ctx.db.GetNodeByID(e.SourceID)
			if err != nil || caller == nil {
				continue
			}
			content := ctx.readFile(caller.File)
			lines := strings.Split(content, "\n")
			if e.Line > len(lines) {
				continue
			}
			line := lines[e.Line-1]
			am := argRe.FindStringSubmatch(line)
			if len(am) < 2 {
				continue
			}
			fn := resolveCallbackTarget(ctx, am[1], caller, reg.node)
			if fn == nil {
				continue
			}
			for _, disp := range chDisp {
				if disp.node.ID == fn.ID {
					continue
				}
				key := edgeKey(disp.node.ID, fn.ID, db.EdgeCalls)
				if seen[key] {
					continue
				}
				seen[key] = true
				edges = append(edges, synthEdge{
					SourceID: disp.node.ID,
					TargetID: fn.ID,
					Kind:     db.EdgeCalls,
					File:     disp.node.File,
					Line:     disp.node.Line,
					Meta: map[string]string{
						"synthesizedBy": "callback",
						"via":           reg.node.Name,
						"field":         reg.field,
						"registeredAt":  caller.File + ":" + itoa(int64(e.Line)),
					},
				})
				added++
			}
		}
	}
	return edges, nil
}

// eventEmitterEdges: emit('e') dispatcher → on('e', handler).
func eventEmitterEdges(ctx *synthCtx) ([]synthEdge, error) {
	type dispHit struct {
		id   int64
		file string
		line int
	}
	emitsByEvent := map[string]map[int64]dispHit{} // event → dispatcher id → hit
	handlersByEvent := map[string]map[int64]string{} // handler id → registeredAt

	for _, file := range ctx.allFiles {
		content := ctx.readFile(file)
		if content == "" {
			continue
		}
		hasEmit := strings.Contains(content, ".emit(") || strings.Contains(content, ".fire(") || strings.Contains(content, ".dispatchEvent(")
		hasOn := strings.Contains(content, ".on(") || strings.Contains(content, ".once(") || strings.Contains(content, ".addListener(")
		if !hasEmit && !hasOn {
			continue
		}
		nodes := ctx.getNodesInFile(file)
		if hasEmit {
			for _, m := range emitEventRe.FindAllStringSubmatchIndex(content, -1) {
				if len(m) < 4 {
					continue
				}
				event := content[m[2]:m[3]]
				line := lineAt(content, m[0])
				disp := enclosingFn(nodes, line)
				if disp == nil {
					continue
				}
				set := emitsByEvent[event]
				if set == nil {
					set = map[int64]dispHit{}
					emitsByEvent[event] = set
				}
				set[disp.ID] = dispHit{id: disp.ID, file: disp.File, line: line}
			}
		}
		if hasOn {
			for _, m := range onEventRe.FindAllStringSubmatchIndex(content, -1) {
				// groups: full, event, functionName?, identName?
				if len(m) < 8 {
					continue
				}
				event := content[m[2]:m[3]]
				handlerName := ""
				if m[4] >= 0 && m[5] >= 0 {
					handlerName = content[m[4]:m[5]]
				} else if m[6] >= 0 && m[7] >= 0 {
					handlerName = content[m[6]:m[7]]
				}
				if handlerName == "" {
					continue
				}
				var handler *db.Node
				for _, n := range ctx.getNodesByName(handlerName) {
					if n.Kind == db.KindFunction || n.Kind == db.KindMethod {
						nn := n
						handler = &nn
						break
					}
				}
				if handler == nil {
					continue
				}
				line := lineAt(content, m[0])
				mp := handlersByEvent[event]
				if mp == nil {
					mp = map[int64]string{}
					handlersByEvent[event] = mp
				}
				mp[handler.ID] = file + ":" + itoa(int64(line))
			}
		}
	}

	var edges []synthEdge
	seen := map[string]bool{}
	for event, dispatchers := range emitsByEvent {
		handlers := handlersByEvent[event]
		if len(handlers) == 0 {
			continue
		}
		if len(dispatchers) > eventFanoutCap || len(handlers) > eventFanoutCap {
			continue
		}
		for d, hit := range dispatchers {
			for h, registeredAt := range handlers {
				if d == h {
					continue
				}
				key := edgeKey(d, h, db.EdgeCalls)
				if seen[key] {
					continue
				}
				seen[key] = true
				edges = append(edges, synthEdge{
					SourceID: d,
					TargetID: h,
					Kind:     db.EdgeCalls,
					File:     hit.file,
					Line:     hit.line,
					Meta: map[string]string{
						"synthesizedBy": "event-emitter",
						"event":         event,
						"registeredAt":  registeredAt,
					},
				})
			}
		}
	}
	return edges, nil
}

// reactRenderEdges: class methods calling this.setState → sibling render().
func reactRenderEdges(ctx *synthCtx) ([]synthEdge, error) {
	classes, err := ctx.nodesOfKind(db.KindClass)
	if err != nil {
		return nil, err
	}
	var edges []synthEdge
	seen := map[string]bool{}
	for _, cls := range classes {
		// Prefer contains edges; fall back to same-file methods inside class line range.
		children := classMethods(ctx, cls)
		var render *db.Node
		for i := range children {
			if children[i].Name == "render" {
				render = &children[i]
				break
			}
		}
		if render == nil {
			continue
		}
		added := 0
		for i := range children {
			m := &children[i]
			if added >= maxCallbacksPerChannel {
				break
			}
			if m.ID == render.ID {
				continue
			}
			src := m.Body
			if src == "" {
				src = sliceLines(ctx.readFile(m.File), m.Line, m.EndLine)
			}
			if src == "" || !setStateRe.MatchString(src) {
				continue
			}
			key := edgeKey(m.ID, render.ID, db.EdgeCalls)
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, synthEdge{
				SourceID: m.ID,
				TargetID: render.ID,
				Kind:     db.EdgeCalls,
				File:     m.File,
				Line:     m.Line,
				Meta: map[string]string{
					"synthesizedBy": "react-render",
					"via":           "setState",
					"registeredAt":  render.File + ":" + itoa(int64(render.Line)),
				},
			})
			added++
		}
	}
	return edges, nil
}

func classMethods(ctx *synthCtx, cls db.Node) []db.Node {
	outEdges, err := ctx.db.GetOutgoingEdges(cls.ID, []string{db.EdgeContains})
	if err == nil && len(outEdges) > 0 {
		var methods []db.Node
		for _, e := range outEdges {
			n, err := ctx.db.GetNodeByID(e.TargetID)
			if err != nil || n == nil || n.Kind != db.KindMethod {
				continue
			}
			methods = append(methods, *n)
		}
		if len(methods) > 0 {
			return methods
		}
	}
	// Fallback: methods in same file within class line span.
	end := cls.EndLine
	if end == 0 {
		end = cls.Line
	}
	var methods []db.Node
	for _, n := range ctx.getNodesInFile(cls.File) {
		if n.Kind != db.KindMethod {
			continue
		}
		if n.Line >= cls.Line && n.Line <= end {
			methods = append(methods, n)
		}
	}
	return methods
}

// reactJSXChildEdges: parent render/function → PascalCase JSX child component.
func reactJSXChildEdges(ctx *synthCtx) ([]synthEdge, error) {
	var edges []synthEdge
	seen := map[string]bool{}
	for _, file := range ctx.allFiles {
		content := ctx.readFile(file)
		if content == "" || (!strings.Contains(content, "</") && !strings.Contains(content, "/>")) {
			continue
		}
		parents := ctx.getNodesInFile(file)
		for i := range parents {
			parent := &parents[i]
			if !isCallableKind(parent.Kind) {
				continue
			}
			src := sliceLines(content, parent.Line, parent.EndLine)
			if src == "" {
				src = parent.Body
			}
			if src == "" || (!strings.Contains(src, "</") && !strings.Contains(src, "/>")) {
				continue
			}
			names := map[string]bool{}
			for _, m := range jsxTagRe.FindAllStringSubmatch(src, -1) {
				if len(m) > 1 {
					names[m[1]] = true
				}
			}
			added := 0
			for name := range names {
				if added >= maxJSXChildren {
					break
				}
				var child *db.Node
				for _, n := range ctx.getNodesByName(name) {
					if n.Kind == "component" || n.Kind == db.KindFunction || n.Kind == db.KindClass {
						nn := n
						child = &nn
						break
					}
				}
				if child == nil || child.ID == parent.ID {
					continue
				}
				key := edgeKey(parent.ID, child.ID, db.EdgeCalls)
				if seen[key] {
					continue
				}
				seen[key] = true
				edges = append(edges, synthEdge{
					SourceID: parent.ID,
					TargetID: child.ID,
					Kind:     db.EdgeCalls,
					File:     parent.File,
					Line:     parent.Line,
					Meta: map[string]string{
						"synthesizedBy": "jsx-render",
						"via":           name,
					},
				})
				added++
			}
		}
	}
	return edges, nil
}

// bridgeSymbolEdges links parked cross-file bridge refs that resolution missed,
// tagging them as synthesized bridge edges when a unique callable target exists.
func bridgeSymbolEdges(ctx *synthCtx) ([]synthEdge, error) {
	pending, err := ctx.db.ListUnresolvedRefs("", "failed")
	if err != nil {
		return nil, err
	}
	pending2, err := ctx.db.ListUnresolvedRefs("", "pending")
	if err != nil {
		return nil, err
	}
	pending = append(pending, pending2...)

	var edges []synthEdge
	seen := map[string]bool{}
	for _, r := range pending {
		if r.ReferenceKind != db.EdgeBridge && r.ReferenceKind != "bridge" {
			continue
		}
		if r.FromNode == 0 || r.ReferenceName == "" {
			continue
		}
		cands, err := CollectCandidates(ctx.db, r.ReferenceName)
		if err != nil || len(cands) == 0 {
			continue
		}
		var callables []db.Node
		for _, c := range cands {
			if c.ID == r.FromNode {
				continue
			}
			if isCallableKind(c.Kind) || c.Kind == db.KindFunction || c.Kind == db.KindMethod {
				callables = append(callables, c)
			}
		}
		// Also accept any uniquely named symbol for native/C targets.
		if len(callables) == 0 && len(cands) == 1 && cands[0].ID != r.FromNode {
			callables = cands
		}
		if len(callables) != 1 {
			continue
		}
		tgt := callables[0]
		key := edgeKey(r.FromNode, tgt.ID, db.EdgeBridge)
		if seen[key] {
			continue
		}
		seen[key] = true
		edges = append(edges, synthEdge{
			SourceID: r.FromNode,
			TargetID: tgt.ID,
			Kind:     db.EdgeBridge,
			File:     r.FilePath,
			Line:     r.Line,
			Meta: map[string]string{
				"synthesizedBy": "bridge",
				"via":           r.ReferenceName,
			},
		})
	}
	return edges, nil
}

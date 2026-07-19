package resolution

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// Must match extraction.GoFrameRouteMarker (kept local to avoid import cycle).
const goFrameRouteMarker = "::goframe-route:"

// GoFrame route → controller method (official goframe-synthesizer).
//
// Join key = request type in handler signature (*pkg.Type / *Type), not method name.
// metadata.synthesizedBy = "goframe-route"

const goframeFanoutCap = 2000

// pointer types in a Go signature: *cash.ListReq → ["cash.ListReq","ListReq"]
var goPointerTypeRe = regexp.MustCompile(`\*\s*(?:(\w+)\.)?([A-Z]\w*)\b`)

func goframeRouteEdges(ctx *synthCtx) ([]synthEdge, error) {
	routes, err := ctx.nodesOfKind("route")
	if err != nil {
		return nil, err
	}
	// joinKey → routes
	byKey := map[string][]db.Node{}
	wanted := map[string]bool{}
	for _, r := range routes {
		if r.Language != "" && r.Language != "go" {
			continue
		}
		qn := r.QualifiedName
		idx := strings.Index(qn, goFrameRouteMarker)
		if idx < 0 {
			continue
		}
		joinKey := qn[idx+len(goFrameRouteMarker):]
		if joinKey == "" {
			continue
		}
		byKey[joinKey] = append(byKey[joinKey], r)
		wanted[joinKey] = true
		if dot := strings.LastIndex(joinKey, "."); dot >= 0 {
			wanted[joinKey[dot+1:]] = true
		}
	}
	if len(byKey) == 0 {
		return nil, nil
	}

	methods, err := ctx.nodesOfKind(db.KindMethod)
	if err != nil {
		return nil, err
	}
	// type key → handler methods
	handlersByKey := map[string][]db.Node{}
	for _, m := range methods {
		if m.Language != "" && m.Language != "go" {
			continue
		}
		if m.Signature == "" {
			continue
		}
		for _, t := range pointerParamTypes(m.Signature) {
			if !wanted[t] {
				continue
			}
			handlersByKey[t] = append(handlersByKey[t], m)
		}
	}

	var edges []synthEdge
	seen := map[string]bool{}
	added := 0
	for joinKey, routeList := range byKey {
		bare := joinKey
		if dot := strings.LastIndex(joinKey, "."); dot >= 0 {
			bare = joinKey[dot+1:]
		}
		cands := handlersByKey[joinKey]
		if len(cands) == 0 {
			cands = handlersByKey[bare]
		}
		if len(cands) == 0 {
			continue
		}
		for _, route := range routeList {
			handler := selectGoFrameHandler(cands, route.File)
			if handler == nil || handler.ID == route.ID {
				continue
			}
			key := edgeKey(route.ID, handler.ID, db.EdgeCalls)
			if seen[key] || added >= goframeFanoutCap {
				continue
			}
			seen[key] = true
			edges = append(edges, synthEdge{
				SourceID: route.ID,
				TargetID: handler.ID,
				Kind:     db.EdgeCalls,
				File:     route.File,
				Line:     route.Line,
				Meta: map[string]string{
					"synthesizedBy": "goframe-route",
					"route":         route.Name,
					"requestType":   bare,
				},
			})
			added++
		}
	}
	return edges, nil
}

func pointerParamTypes(sig string) []string {
	var out []string
	for _, m := range goPointerTypeRe.FindAllStringSubmatch(sig, -1) {
		if len(m) < 3 {
			continue
		}
		if m[1] != "" {
			out = append(out, m[1]+"."+m[2])
		}
		out = append(out, m[2])
	}
	return out
}

// Prefer controller/ dir, then same addon module; ambiguity → nil (silent > wrong).
func selectGoFrameHandler(cands []db.Node, routeFile string) *db.Node {
	if len(cands) == 1 {
		return &cands[0]
	}
	var tmp []db.Node
	for i := range cands {
		h := &cands[i]
		p := filepath.ToSlash(h.File)
		if strings.Contains(p, "/controller/") || strings.Contains(p, "/controllers/") {
			tmp = append(tmp, *h)
		}
	}
	if len(tmp) == 1 {
		return &tmp[0]
	}
	if len(tmp) == 0 {
		tmp = cands
	}
	ar := addonRoot(routeFile)
	var same []db.Node
	for i := range tmp {
		if addonRoot(tmp[i].File) == ar {
			same = append(same, tmp[i])
		}
	}
	if len(same) == 1 {
		return &same[0]
	}
	return nil
}

func addonRoot(p string) string {
	p = filepath.ToSlash(p)
	const mark = "/addons/"
	i := strings.Index(p, mark)
	if i < 0 {
		return ""
	}
	rest := p[i+len(mark):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return rest
}

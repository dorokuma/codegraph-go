package extraction

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Frontend SFC helpers for vue / svelte / astro.
// Strategy (official-aligned, simplified): pull script(+frontmatter) blocks,
// run JS/TS extraction on them with line offsets, emit a file-level component
// node, and collect PascalCase / kebab-case template component usages as refs.

var (
	sfcScriptRe = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	sfcStyleRe  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	sfcTplRe    = regexp.MustCompile(`(?is)<template[^>]*>(.*?)</template>`)
	// Opening tags only (not </close>).
	sfcOpenTagRe = regexp.MustCompile(`<([A-Za-z][\w-]*)\b`)
	// Astro frontmatter: leading --- … ---
	astroFMRe = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---`)
	// Template expression calls: {foo(} / {foo.bar(} / @click="foo(" / v-on:click="foo("
	tplCallRe = regexp.MustCompile(`(?:\{[\s\n]*|@\w+(?:\.\w+)*\s*=\s*["']|v-on:\w+\s*=\s*["'])([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s*\(`)
)

// Framework built-in components (after kebab→Pascal). Not user code.
var sfcBuiltinComponents = map[string]struct{}{
	"Transition": {}, "TransitionGroup": {}, "KeepAlive": {},
	"Suspense": {}, "Teleport": {}, "Component": {}, "Slot": {},
	"Fragment": {}, "Code": {}, "Debug": {},
	// Vue router / common framework shells
	"RouterView": {}, "RouterLink": {}, "NuxtLink": {}, "NuxtPage": {},
}

// HTML/SVG native element names (lowercase). Never component refs.
var htmlNativeElements = map[string]struct{}{
	"a": {}, "abbr": {}, "address": {}, "area": {}, "article": {}, "aside": {},
	"audio": {}, "b": {}, "base": {}, "bdi": {}, "bdo": {}, "blockquote": {},
	"body": {}, "br": {}, "button": {}, "canvas": {}, "caption": {}, "cite": {},
	"code": {}, "col": {}, "colgroup": {}, "data": {}, "datalist": {}, "dd": {},
	"del": {}, "details": {}, "dfn": {}, "dialog": {}, "div": {}, "dl": {},
	"dt": {}, "em": {}, "embed": {}, "fieldset": {}, "figcaption": {}, "figure": {},
	"footer": {}, "form": {}, "h1": {}, "h2": {}, "h3": {}, "h4": {}, "h5": {},
	"h6": {}, "head": {}, "header": {}, "hgroup": {}, "hr": {}, "html": {},
	"i": {}, "iframe": {}, "img": {}, "input": {}, "ins": {}, "kbd": {},
	"label": {}, "legend": {}, "li": {}, "link": {}, "main": {}, "map": {},
	"mark": {}, "menu": {}, "meta": {}, "meter": {}, "nav": {}, "noscript": {},
	"object": {}, "ol": {}, "optgroup": {}, "option": {}, "output": {}, "p": {},
	"param": {}, "picture": {}, "pre": {}, "progress": {}, "q": {}, "rp": {},
	"rt": {}, "ruby": {}, "s": {}, "samp": {}, "script": {}, "search": {},
	"section": {}, "select": {}, "slot": {}, "small": {}, "source": {}, "span": {},
	"strong": {}, "style": {}, "sub": {}, "summary": {}, "sup": {}, "svg": {},
	"table": {}, "tbody": {}, "td": {}, "template": {}, "textarea": {}, "tfoot": {},
	"th": {}, "thead": {}, "time": {}, "title": {}, "tr": {}, "track": {},
	"u": {}, "ul": {}, "var": {}, "video": {}, "wbr": {},
	// common SVG children
	"path": {}, "g": {}, "circle": {}, "rect": {}, "line": {}, "polyline": {},
	"polygon": {}, "ellipse": {}, "text": {}, "tspan": {}, "defs": {}, "use": {},
	"symbol": {}, "clippath": {}, "mask": {}, "pattern": {}, "image": {},
	"foreignobject": {}, "lineargradient": {}, "radialgradient": {}, "stop": {},
}

type sfcScriptBlock struct {
	content    string
	lineOffset int // add to 1-based lines from inner extract (startLine-1)
	lang       string
}

// extractSFC builds a full ExtractResult for vue/svelte/astro files.
func (e *Extractor) extractSFC(source, filePath string) ExtractResult {
	compName := sfcComponentName(filePath)
	endLine := strings.Count(source, "\n") + 1
	if endLine < 1 {
		endLine = 1
	}

	out := ExtractResult{
		Nodes: []ExtractedNode{{
			Kind:          "component",
			Name:          compName,
			File:          filePath,
			Line:          1,
			EndLine:       endLine,
			Body:          "",
			Language:      e.language,
			QualifiedName: compName,
			Visibility:    "public",
			IsExported:    true, // SFC components are always importable
		}},
	}

	blocks := findSFCScripts(source)
	if e.language == "astro" {
		if fm, ok := findAstroFrontmatter(source); ok {
			blocks = append([]sfcScriptBlock{fm}, blocks...)
		}
	}

	for _, b := range blocks {
		part := extractScriptContent(b.lang, b.content, filePath)
		for _, n := range part.Nodes {
			n.Language = e.language
			n.Line = shiftLine(n.Line, b.lineOffset)
			n.EndLine = shiftLine(n.EndLine, b.lineOffset)
			out.Nodes = append(out.Nodes, n)
		}
		for _, edge := range part.Edges {
			edge.Line = shiftLine(edge.Line, b.lineOffset)
			out.Edges = append(out.Edges, edge)
		}
		for _, ref := range part.Refs {
			if IsNoisyRefName(ref.ReferenceName) {
				continue
			}
			ref.Language = e.language
			ref.FilePath = filePath
			ref.Line = shiftLine(ref.Line, b.lineOffset)
			ref.FromLine = shiftLine(ref.FromLine, b.lineOffset)
			out.Refs = append(out.Refs, ref)
		}
	}

	// Template component usages + expression calls.
	for _, ref := range extractSFCTemplateRefs(e.language, source, filePath, compName) {
		if IsNoisyRefName(ref.ReferenceName) {
			continue
		}
		out.Refs = append(out.Refs, ref)
	}
	return out
}

func sfcComponentName(filePath string) string {
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if name == "" {
		return "Component"
	}
	return name
}

func shiftLine(line, offset int) int {
	if line <= 0 {
		return line
	}
	return line + offset
}

func findSFCScripts(source string) []sfcScriptBlock {
	var out []sfcScriptBlock
	for _, m := range sfcScriptRe.FindAllStringSubmatchIndex(source, -1) {
		if len(m) < 6 {
			continue
		}
		attrs := source[m[2]:m[3]]
		body := source[m[4]:m[5]]
		if strings.TrimSpace(body) == "" {
			continue
		}
		startLine := 1
		if m[4] > 0 {
			startLine = strings.Count(source[:m[4]], "\n") + 1
		}
		out = append(out, sfcScriptBlock{
			content:    body,
			lineOffset: startLine - 1,
			lang:       scriptLangFromAttrs(attrs),
		})
	}
	return out
}

func findAstroFrontmatter(source string) (sfcScriptBlock, bool) {
	m := astroFMRe.FindStringSubmatchIndex(source)
	if m == nil || len(m) < 4 {
		return sfcScriptBlock{}, false
	}
	body := source[m[2]:m[3]]
	if strings.TrimSpace(body) == "" {
		return sfcScriptBlock{}, false
	}
	// body starts after "---\n"
	startLine := 2 // line after opening ---
	return sfcScriptBlock{
		content:    body,
		lineOffset: startLine - 1,
		lang:       "typescript", // Astro frontmatter is TS by default
	}, true
}

func scriptLangFromAttrs(attrs string) string {
	low := strings.ToLower(attrs)
	// lang="ts" / lang='tsx' / lang=typescript
	if strings.Contains(low, "lang=\"ts\"") || strings.Contains(low, "lang='ts'") ||
		strings.Contains(low, "lang=\"tsx\"") || strings.Contains(low, "lang='tsx'") ||
		strings.Contains(low, "lang=\"typescript\"") || strings.Contains(low, "lang='typescript'") ||
		strings.Contains(low, "lang=ts") || strings.Contains(low, "lang=tsx") ||
		strings.Contains(low, "lang=typescript") {
		return "typescript"
	}
	return "javascript"
}

// extractScriptContent runs tree-sitter (preferred) or regex JS/TS extract.
func extractScriptContent(lang, content, filePath string) ExtractResult {
	if ts := NewTreeSitterExtractor(lang); ts != nil {
		return ts.Extract(content, filePath)
	}
	return NewExtractor(lang).Extract(content, filePath)
}

func extractSFCTemplateRefs(language, source, filePath, fromComponent string) []UnresolvedReference {
	var regions []struct {
		text   string
		offset int // line offset (startLine-1)
	}

	switch language {
	case "vue":
		for _, m := range sfcTplRe.FindAllStringSubmatchIndex(source, -1) {
			if len(m) < 4 {
				continue
			}
			body := source[m[2]:m[3]]
			startLine := 1
			if m[2] > 0 {
				startLine = strings.Count(source[:m[2]], "\n") + 1
			}
			regions = append(regions, struct {
				text   string
				offset int
			}{body, startLine - 1})
		}
	default:
		// svelte / astro: whole file minus scripts/styles/(astro fm)
		stripped := sfcScriptRe.ReplaceAllString(source, "\n")
		stripped = sfcStyleRe.ReplaceAllString(stripped, "\n")
		if language == "astro" {
			stripped = astroFMRe.ReplaceAllString(stripped, "\n")
		}
		regions = append(regions, struct {
			text   string
			offset int
		}{stripped, 0})
	}

	var refs []UnresolvedReference
	seen := map[string]bool{}
	for _, reg := range regions {
		// Component opening tags only.
		for _, m := range sfcOpenTagRe.FindAllStringSubmatchIndex(reg.text, -1) {
			if len(m) < 4 {
				continue
			}
			// Reject matches that are actually close tags (safety if regex drifts).
			if m[2] > 0 && reg.text[m[2]-1] == '/' {
				continue
			}
			raw := reg.text[m[2]:m[3]]
			name := normalizeTemplateComponent(raw)
			if name == "" || name == fromComponent {
				continue
			}
			if isNativeHTMLElement(raw) {
				continue
			}
			if _, skip := sfcBuiltinComponents[name]; skip {
				continue
			}
			// User components: PascalCase, or kebab custom elements → Pascal.
			if name[0] < 'A' || name[0] > 'Z' {
				continue
			}
			key := "cmp:" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			line := 1
			if m[2] > 0 {
				line = strings.Count(reg.text[:m[2]], "\n") + 1
			}
			refs = append(refs, UnresolvedReference{
				FromName:      fromComponent,
				FromLine:      1,
				ReferenceName: name,
				ReferenceKind: "references",
				Line:          shiftLine(line, reg.offset),
				FilePath:      filePath,
				Language:      language,
			})
		}
		// Expression / handler calls
		for _, m := range tplCallRe.FindAllStringSubmatchIndex(reg.text, -1) {
			if len(m) < 4 {
				continue
			}
			raw := reg.text[m[2]:m[3]]
			name := raw
			if i := strings.LastIndex(raw, "."); i >= 0 {
				name = raw[i+1:]
			}
			if name == "" || IsNoisyRefName(name) {
				continue
			}
			key := "call:" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			line := 1
			if m[2] > 0 {
				line = strings.Count(reg.text[:m[2]], "\n") + 1
			}
			refs = append(refs, UnresolvedReference{
				FromName:      fromComponent,
				FromLine:      1,
				ReferenceName: name,
				ReferenceKind: "calls",
				Line:          shiftLine(line, reg.offset),
				FilePath:      filePath,
				Language:      language,
			})
		}
	}
	return refs
}

func isNativeHTMLElement(raw string) bool {
	_, ok := htmlNativeElements[strings.ToLower(strings.TrimSpace(raw))]
	return ok
}

func normalizeTemplateComponent(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// kebab-case / custom elements → PascalCase (vue allows either)
	if strings.Contains(raw, "-") {
		return kebabToPascal(raw)
	}
	return raw
}

func kebabToPascal(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

package extraction

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"
	c "github.com/smacker/go-tree-sitter/c"
	cpp "github.com/smacker/go-tree-sitter/cpp"
	csharp "github.com/smacker/go-tree-sitter/csharp"
	golang "github.com/smacker/go-tree-sitter/golang"
	java "github.com/smacker/go-tree-sitter/java"
	kotlin "github.com/smacker/go-tree-sitter/kotlin"
	lua "github.com/smacker/go-tree-sitter/lua"
	php "github.com/smacker/go-tree-sitter/php"
	python "github.com/smacker/go-tree-sitter/python"
	ruby "github.com/smacker/go-tree-sitter/ruby"
	rust "github.com/smacker/go-tree-sitter/rust"
	scala "github.com/smacker/go-tree-sitter/scala"
	swift "github.com/smacker/go-tree-sitter/swift"
	typescript "github.com/smacker/go-tree-sitter/typescript/tsx"
)

// TreeSitterExtractor uses tree-sitter for AST-based extraction.
type TreeSitterExtractor struct {
	language string
	lang     *sitter.Language
}

// treeSitterLangFactory pairs a language name with its grammar getter.
type treeSitterLangFactory struct {
	get func() *sitter.Language
}

// registry is populated by init() in treesitter_extended.go and the built-in set below.
var tsLangRegistry = map[string]treeSitterLangFactory{
	"go":       {get: golang.GetLanguage},
	"typescript": {get: typescript.GetLanguage},
	"javascript": {get: typescript.GetLanguage},
	"python":    {get: python.GetLanguage},
	"c":         {get: c.GetLanguage},
	"cpp":       {get: cpp.GetLanguage},
	"java":      {get: java.GetLanguage},
	"kotlin":    {get: kotlin.GetLanguage},
	"rust":      {get: rust.GetLanguage},
	"ruby":      {get: ruby.GetLanguage},
	"php":       {get: php.GetLanguage},
	"csharp":    {get: csharp.GetLanguage},
	"scala":     {get: scala.GetLanguage},
	"swift":     {get: swift.GetLanguage},
	"lua":       {get: lua.GetLanguage},
}

// tsParserPools caches sync.Pool per language so parsers are reused across files
// instead of being created per-file (and left for GC non-deterministic finalization).
var tsParserPools sync.Map

func getParserPool(langName string) *sync.Pool {
	if p, ok := tsParserPools.Load(langName); ok {
		return p.(*sync.Pool)
	}
	f, ok := tsLangRegistry[langName]
	if !ok {
		return nil
	}
	pool := &sync.Pool{
		New: func() any {
			p := sitter.NewParser()
			p.SetLanguage(f.get())
			return p
		},
	}
	actual, _ := tsParserPools.LoadOrStore(langName, pool)
	return actual.(*sync.Pool)
}

// NewTreeSitterExtractor creates a tree-sitter extractor for the given language.
// Parsers are obtained from a per-language pool on each Extract call.
func NewTreeSitterExtractor(language string) *TreeSitterExtractor {
	factory, ok := tsLangRegistry[language]
	if !ok {
		return nil
	}
	lang := factory.get()
	return &TreeSitterExtractor{language: language, lang: lang}
}

// Extract parses the source code and returns nodes and edges using tree-sitter.
func (e *TreeSitterExtractor) Extract(source string, filePath string) ExtractResult {
	if e.lang == nil {
		return ExtractResult{}
	}
	pool := getParserPool(e.language)
	if pool == nil {
		return ExtractResult{}
	}
	parser := pool.Get().(*sitter.Parser)
	defer pool.Put(parser)

	sourceBytes := []byte(source)
	tree, err := parser.ParseCtx(context.Background(), nil, sourceBytes)
	if err != nil {
		return ExtractResult{}
	}
	defer tree.Close()

	root := tree.RootNode()

	var nodes []ExtractedNode
	var edges []ExtractedEdge
	switch e.language {
	case "go":
		nodes, edges = e.extractGo(root, sourceBytes, filePath)
	case "typescript", "javascript":
		nodes, edges = e.extractJS(root, sourceBytes, filePath)
	case "python":
		nodes, edges = e.extractPython(root, sourceBytes, filePath)
	case "rust":
		nodes, edges = e.extractRust(root, sourceBytes, filePath)
	case "ruby":
		nodes, edges = e.extractRuby(root, sourceBytes, filePath)
	case "php":
		nodes, edges = e.extractPHP(root, sourceBytes, filePath)
	case "c", "cpp", "java", "kotlin", "csharp", "scala":
		nodes, edges = e.extractCLike(root, sourceBytes, filePath)
	case "swift":
		nodes, edges = e.extractCLike(root, sourceBytes, filePath)
	case "lua":
		nodes, edges = e.extractLua(root, sourceBytes, filePath)
	default:
		return ExtractResult{}
	}
	return promoteCallsToRefs(nodes, edges, filePath, e.language)
}

// ---------- Go extraction ----------

var goReceiverTypeRe = regexp.MustCompile(`\(\s*(?:[A-Za-z_]\w*\s+)?\*?\s*([A-Za-z_]\w*)`)

func (e *TreeSitterExtractor) extractGo(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	pkg := goPackageName(root, source)
	e.walkGo(root, source, filePath, pkg, &nodes, &edges)
	return nodes, edges
}

func goPackageName(root *sitter.Node, source []byte) string {
	for i := 0; i < int(root.ChildCount()); i++ {
		ch := root.Child(i)
		if ch.Type() != "package_clause" {
			continue
		}
		for j := 0; j < int(ch.ChildCount()); j++ {
			c := ch.Child(j)
			if c.Type() == "package_identifier" {
				return c.Content(source)
			}
		}
	}
	return ""
}

func goIsExported(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return unicode.IsUpper(r)
}

func goVisibility(name string) string {
	if goIsExported(name) {
		return "public"
	}
	return "private"
}

func goQualified(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

func goMethodQualified(receiver, name string) string {
	if receiver == "" {
		return name
	}
	return receiver + "." + name
}

func goSignatureAndReturn(node *sitter.Node, source []byte) (sig, ret string) {
	params := node.ChildByFieldName("parameters")
	if params != nil {
		sig = params.Content(source)
	}
	result := node.ChildByFieldName("result")
	if result == nil {
		return sig, ""
	}
	sig = strings.TrimSpace(sig + " " + result.Content(source))
	ret = goNormalizeReturnType(result, source)
	return sig, ret
}

func goNormalizeReturnType(result *sitter.Node, source []byte) string {
	n := result
	// Multi-return (T, error) → first result type.
	if n.Type() == "parameter_list" {
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			if c != nil && c.Type() == "parameter_declaration" {
				if t := c.ChildByFieldName("type"); t != nil {
					n = t
				} else {
					n = c
				}
				break
			}
		}
	}
	if n.Type() == "pointer_type" {
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "type_identifier", "qualified_type", "generic_type":
				n = c
			}
		}
	}
	text := strings.TrimSpace(n.Content(source))
	text = strings.TrimPrefix(text, "*")
	// Strip generic/array bits lightly.
	if i := strings.IndexAny(text, "[<"); i >= 0 {
		text = text[:i]
	}
	if i := strings.LastIndex(text, "."); i >= 0 {
		text = text[i+1:]
	}
	text = strings.TrimSpace(text)
	if text == "" || !isIdentStart(text) {
		return ""
	}
	return text
}

func isIdentStart(s string) bool {
	r := []rune(s)[0]
	return unicode.IsLetter(r) || r == '_'
}

func goReceiverType(node *sitter.Node, source []byte) string {
	recv := node.ChildByFieldName("receiver")
	if recv == nil {
		return ""
	}
	m := goReceiverTypeRe.FindStringSubmatch(recv.Content(source))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func (e *TreeSitterExtractor) walkGo(node *sitter.Node, source []byte, filePath, pkg string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	switch node.Type() {
	case "function_declaration":
		e.processGoFunction(node, source, filePath, pkg, "function", nodes, edges)
	case "method_declaration":
		e.processGoFunction(node, source, filePath, pkg, "method", nodes, edges)
	case "type_declaration":
		e.processGoTypeDecl(node, source, filePath, pkg, nodes)
	case "import_declaration":
		e.processImport(node, source, filePath, edges)
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkGo(node.Child(i), source, filePath, pkg, nodes, edges)
	}
}

func (e *TreeSitterExtractor) processGoFunction(node *sitter.Node, source []byte, filePath, pkg, kind string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	sig, ret := goSignatureAndReturn(node, source)
	exported := goIsExported(name)
	qn := goQualified(pkg, name)
	receiver := ""
	if kind == "method" {
		receiver = goReceiverType(node, source)
		qn = goMethodQualified(receiver, name)
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind:          kind,
		Name:          name,
		File:          filePath,
		Line:          startLine,
		EndLine:       endLine,
		Body:          node.Content(source),
		Language:      "go",
		QualifiedName: qn,
		Signature:     sig,
		Visibility:    goVisibility(name),
		IsExported:    exported,
		ReturnType:    ret,
		StartColumn:   int(node.StartPoint().Column),
		EndColumn:     int(node.EndPoint().Column),
	})
	if kind == "method" && receiver != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: receiver,
			TargetName: name,
			Kind:       "contains",
			File:       filePath,
			Line:       startLine,
		})
	}
	e.extractCallsFromNode(node, source, filePath, name, edges)
}

func (e *TreeSitterExtractor) processGoTypeDecl(node *sitter.Node, source []byte, filePath, pkg string, nodes *[]ExtractedNode) {
	// type_declaration → type_spec children (name/type live on type_spec).
	for i := 0; i < int(node.ChildCount()); i++ {
		spec := node.Child(i)
		if spec.Type() != "type_spec" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nameNode.Content(source)
		kind := "type"
		if typeNode := spec.ChildByFieldName("type"); typeNode != nil {
			switch typeNode.Type() {
			case "struct_type":
				kind = "struct"
			case "interface_type":
				kind = "interface"
			}
		}
		startLine := int(spec.StartPoint().Row) + 1
		endLine := int(spec.EndPoint().Row) + 1
		*nodes = append(*nodes, ExtractedNode{
			Kind:          kind,
			Name:          name,
			File:          filePath,
			Line:          startLine,
			EndLine:       endLine,
			Body:          spec.Content(source),
			Language:      "go",
			QualifiedName: goQualified(pkg, name),
			Visibility:    goVisibility(name),
			IsExported:    goIsExported(name),
			StartColumn:   int(spec.StartPoint().Column),
			EndColumn:     int(spec.EndPoint().Column),
		})
	}
}

func (e *TreeSitterExtractor) extractCallsFromNode(funcNode *sitter.Node, source []byte, filePath string, funcName string, edges *[]ExtractedEdge) {
	bodyNode := funcNode.ChildByFieldName("body")
	if bodyNode == nil {
		return
	}
	e.findCalls(bodyNode, source, filePath, funcName, edges, make(map[string]bool))
}

func (e *TreeSitterExtractor) findCalls(node *sitter.Node, source []byte, filePath string, funcName string, edges *[]ExtractedEdge, seen map[string]bool) {
	// Stop at named nested function declarations (their calls belong to them).
	// Anonymous func_literal bodies are traversed — their calls belong to the outer function.
	if node.Type() == "function_declaration" || node.Type() == "method_declaration" {
		return
	}

	if node.Type() == "call_expression" {
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			calleeName := ""
			if funcNode.Type() == "identifier" {
				calleeName = funcNode.Content(source)
			} else if funcNode.Type() == "selector_expression" {
				// method call like obj.Method()
				fieldNode := funcNode.ChildByFieldName("field")
				if fieldNode != nil {
					calleeName = fieldNode.Content(source)
				}
			}

			if calleeName != "" && calleeName != funcName && !isGoKeyword(calleeName) {
				callLine := int(node.StartPoint().Row) + 1
				key := fmt.Sprintf("%s:%d", calleeName, callLine)
				if !seen[key] {
					seen[key] = true
					*edges = append(*edges, ExtractedEdge{
						SourceName: funcName,
						TargetName: calleeName,
						Kind:       "calls",
						File:       filePath,
						Line:       callLine,
					})
				}
			}
		}
	}

	// Recurse
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.findCalls(child, source, filePath, funcName, edges, seen)
	}
}

func (e *TreeSitterExtractor) processImport(node *sitter.Node, source []byte, filePath string, edges *[]ExtractedEdge) {
	// Go import_declaration has an import_spec child with a string literal
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "import_spec" {
			// Get the path string literal
			pathNode := child.ChildByFieldName("path")
			if pathNode == nil {
				// Try first child
				for j := 0; j < int(child.ChildCount()); j++ {
					c := child.Child(j)
					if c.Type() == "interpreted_string_literal" {
						pathNode = c
						break
					}
				}
			}
			if pathNode != nil {
				importPath := pathNode.Content(source)
				importPath = strings.Trim(importPath, "\"")
				if importPath != "" {
					*edges = append(*edges, ExtractedEdge{
						SourceName: filePath,
						TargetName: importPath,
						Kind:       "imports",
						File:       filePath,
						Line:       int(node.StartPoint().Row) + 1,
					})
				}
			}
		}
	}
}

// ---------- TypeScript/JavaScript extraction ----------

func (e *TreeSitterExtractor) extractJS(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	e.walkJS(root, source, filePath, &nodes, &edges, "")

	return nodes, edges
}

// walkJS walks the JS/TS AST. enclosingClass is non-empty while inside a class body
// so method_definition nodes can emit class→method contains edges.
func (e *TreeSitterExtractor) walkJS(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingClass string) {
	switch node.Type() {
	case "function_declaration":
		e.processJSFunction(node, source, filePath, nodes, edges)
		e.walkNestedNamedJS(node, source, filePath, nodes, edges)
		return
	case "function_expression", "arrow_function":
		// Named function expressions only (anonymous arrows handled via variable_declarator).
		e.processJSFunction(node, source, filePath, nodes, edges)
		e.walkNestedNamedJS(node, source, filePath, nodes, edges)
		return
	case "class_declaration":
		className := e.processJSClass(node, source, filePath, nodes)
		// Walk class body with class context so methods become method nodes.
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkJS(body.Child(i), source, filePath, nodes, edges, className)
			}
		}
		return
	case "method_definition":
		e.processJSMethod(node, source, filePath, nodes, edges, enclosingClass)
		return
	case "variable_declarator":
		e.processJSVarFunction(node, source, filePath, nodes, edges)
		// Still walk value for nested named functions inside the initializer.
	case "import_statement":
		e.processJSImport(node, source, filePath, edges)
		return
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkJS(node.Child(i), source, filePath, nodes, edges, enclosingClass)
	}
}

// walkNestedNamedJS extracts named nested functions (EventEmitter handlers etc.).
func (e *TreeSitterExtractor) walkNestedNamedJS(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "function_declaration", "function_expression":
			if child.ChildByFieldName("name") != nil {
				e.processJSFunction(child, source, filePath, nodes, edges)
				e.walkNestedNamedJS(child, source, filePath, nodes, edges)
			}
		default:
			// Don't re-enter arrow/function bodies as top-level walk — only hunt nested decls.
			if child.Type() == "arrow_function" {
				continue
			}
			e.walkNestedNamedJS(child, source, filePath, nodes, edges)
		}
	}
}

func jsIsExported(node *sitter.Node) bool {
	for cur := node.Parent(); cur != nil; cur = cur.Parent() {
		if cur.Type() == "export_statement" {
			return true
		}
	}
	return false
}

func jsSignatureAndReturn(node *sitter.Node, source []byte) (sig, ret string) {
	params := node.ChildByFieldName("parameters")
	if params == nil {
		// arrow with single param: `x => ...` has parameter field, not parameters
		if p := node.ChildByFieldName("parameter"); p != nil {
			return "(" + p.Content(source) + ")", ""
		}
		return "", ""
	}
	sig = params.Content(source)
	rt := node.ChildByFieldName("return_type")
	if rt == nil {
		return sig, ""
	}
	raw := strings.TrimSpace(rt.Content(source))
	raw = strings.TrimPrefix(raw, ":")
	raw = strings.TrimSpace(raw)
	sig = sig + ": " + raw
	return sig, raw
}

func jsVisibility(node *sitter.Node, source []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		ch := node.Child(i)
		if ch != nil && ch.Type() == "accessibility_modifier" {
			switch ch.Content(source) {
			case "public", "private", "protected":
				return ch.Content(source)
			}
		}
	}
	return ""
}

func jsQualified(className, name string) string {
	if className == "" {
		return name
	}
	return className + "." + name
}

func (e *TreeSitterExtractor) processJSFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	// Prefer export_statement ancestor on the declaration itself.
	exported := jsIsExported(node)
	e.appendJSCallable("function", name, node, source, filePath, nodes, edges, exported, "")
}

// processJSVarFunction handles `const Foo = () => {}` / `const bar = function(){}`.
func (e *TreeSitterExtractor) processJSVarFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	valNode := node.ChildByFieldName("value")
	if nameNode == nil || valNode == nil {
		return
	}
	switch valNode.Type() {
	case "arrow_function", "function_expression", "generator_function":
		name := nameNode.Content(source)
		if name == "" {
			return
		}
		kind := "function"
		if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
			kind = "component"
		}
		// export lives on the lexical_declaration / export_statement above the declarator.
		exported := jsIsExported(node)
		e.appendJSCallable(kind, name, valNode, source, filePath, nodes, edges, exported, "")
		e.walkNestedNamedJS(valNode, source, filePath, nodes, edges)
	}
}

func (e *TreeSitterExtractor) processJSClass(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode) string {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	exported := jsIsExported(node)
	vis := ""
	if exported {
		vis = "public"
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind:          "class",
		Name:          name,
		File:          filePath,
		Line:          startLine,
		EndLine:       endLine,
		Body:          node.Content(source),
		Language:      e.language,
		QualifiedName: name,
		Visibility:    vis,
		IsExported:    exported,
		StartColumn:   int(node.StartPoint().Column),
		EndColumn:     int(node.EndPoint().Column),
	})
	return name
}

func (e *TreeSitterExtractor) processJSMethod(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, className string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	if name == "" {
		return
	}
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	sig, ret := jsSignatureAndReturn(node, source)
	vis := jsVisibility(node, source)
	// Class export bubbles to members when no explicit modifier.
	exported := vis == "public" || (vis == "" && className != "" && jsClassExported(nodes, className))
	if vis == "private" {
		exported = false
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind:          "method",
		Name:          name,
		File:          filePath,
		Line:          startLine,
		EndLine:       endLine,
		Body:          node.Content(source),
		Language:      e.language,
		QualifiedName: jsQualified(className, name),
		Signature:     sig,
		Visibility:    vis,
		IsExported:    exported,
		ReturnType:    ret,
		StartColumn:   int(node.StartPoint().Column),
		EndColumn:     int(node.EndPoint().Column),
	})
	if className != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: className,
			TargetName: name,
			Kind:       "contains",
			File:       filePath,
			Line:       startLine,
		})
	}
	body := node.ChildByFieldName("body")
	if body == nil {
		body = node
	}
	e.findCallsJS(body, source, filePath, name, edges, make(map[string]bool))
	e.walkNestedNamedJS(body, source, filePath, nodes, edges)
}

func jsClassExported(nodes *[]ExtractedNode, className string) bool {
	for _, n := range *nodes {
		if n.Kind == "class" && n.Name == className {
			return n.IsExported
		}
	}
	return false
}

func (e *TreeSitterExtractor) appendJSCallable(kind, name string, node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, exported bool, className string) {
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	sig, ret := jsSignatureAndReturn(node, source)
	vis := ""
	if exported {
		vis = "public"
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind:          kind,
		Name:          name,
		File:          filePath,
		Line:          startLine,
		EndLine:       endLine,
		Body:          node.Content(source),
		Language:      e.language,
		QualifiedName: jsQualified(className, name),
		Signature:     sig,
		Visibility:    vis,
		IsExported:    exported,
		ReturnType:    ret,
		StartColumn:   int(node.StartPoint().Column),
		EndColumn:     int(node.EndPoint().Column),
	})
	body := node.ChildByFieldName("body")
	if body == nil {
		body = node
	}
	e.findCallsJS(body, source, filePath, name, edges, make(map[string]bool))
}

// findCallsJS records call edges with the *call-site* line (needed by callback synthesis).
func (e *TreeSitterExtractor) findCallsJS(node *sitter.Node, source []byte, filePath string, funcName string, edges *[]ExtractedEdge, seen map[string]bool) {
	// Stop at named function declarations — their calls belong to them.
	// Anonymous arrow_function/function_expression bodies are traversed — their calls belong to the outer function.
	if node.Type() == "function_declaration" {
		return
	}
	// Named function_expression (e.g. const f = function foo(){...}) already has
	// its own node created by walkNestedNamedJS; skip its body to avoid double-counting.
	if node.Type() == "function_expression" && node.ChildByFieldName("name") != nil {
		return
	}

	if node.Type() == "call_expression" {
		funcNode := node.ChildByFieldName("function")
		if funcNode == nil {
			funcNode = node.Child(0)
		}
		if funcNode != nil {
			calleeName := ""
			if funcNode.Type() == "identifier" {
				calleeName = funcNode.Content(source)
			} else if funcNode.Type() == "member_expression" {
				propNode := funcNode.ChildByFieldName("property")
				if propNode != nil {
					calleeName = propNode.Content(source)
				}
			}

			if calleeName != "" && calleeName != funcName && !isJSKeyword(calleeName) {
				callLine := int(node.StartPoint().Row) + 1
				key := fmt.Sprintf("%s:%d", calleeName, callLine)
				if !seen[key] {
					seen[key] = true
					*edges = append(*edges, ExtractedEdge{
						SourceName: funcName,
						TargetName: calleeName,
						Kind:       "calls",
						File:       filePath,
						Line:       callLine,
					})
				}
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		e.findCallsJS(node.Child(i), source, filePath, funcName, edges, seen)
	}
}

func (e *TreeSitterExtractor) processJSImport(node *sitter.Node, source []byte, filePath string, edges *[]ExtractedEdge) {
	// Get import source
	sourceNode := node.ChildByFieldName("source")
	if sourceNode == nil {
		return
	}
	importPath := sourceNode.Content(source)
	importPath = strings.Trim(importPath, "\"'")

	*edges = append(*edges, ExtractedEdge{
		SourceName: filePath,
		TargetName: importPath,
		Kind:       "imports",
		File:       filePath,
		Line:       int(node.StartPoint().Row) + 1,
	})
}

// ---------- Python extraction ----------

func pyIsExported(name string) bool {
	return name != "" && !strings.HasPrefix(name, "_")
}

func pyVisibility(name string) string {
	if strings.HasPrefix(name, "__") && !strings.HasSuffix(name, "__") {
		return "private"
	}
	if strings.HasPrefix(name, "_") {
		return "protected"
	}
	return "public"
}

func pySignatureAndReturn(node *sitter.Node, source []byte) (sig, ret string) {
	params := node.ChildByFieldName("parameters")
	if params != nil {
		sig = params.Content(source)
	}
	rt := node.ChildByFieldName("return_type")
	if rt == nil {
		return sig, ""
	}
	ret = strings.TrimSpace(rt.Content(source))
	sig = strings.TrimSpace(sig + " -> " + ret)
	return sig, ret
}

func pyQualified(className, name string) string {
	if className == "" {
		return name
	}
	return className + "." + name
}

func (e *TreeSitterExtractor) extractPython(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	e.walkPython(root, source, filePath, "", &nodes, &edges)
	return nodes, edges
}

// walkPython walks the Python AST. enclosingClass is set while inside a class body
// so nested function_definition nodes become methods with Class.name qualified names.
func (e *TreeSitterExtractor) walkPython(node *sitter.Node, source []byte, filePath, enclosingClass string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	switch node.Type() {
	case "function_definition":
		e.processPythonFunction(node, source, filePath, enclosingClass, nodes, edges)
		// Nested defs inside this function are walked with no class context.
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkPython(body.Child(i), source, filePath, "", nodes, edges)
			}
		}
		return
	case "class_definition":
		className := e.processPythonClass(node, source, filePath, nodes)
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkPython(body.Child(i), source, filePath, className, nodes, edges)
			}
		}
		return
	case "import_statement", "import_from_statement":
		e.processPythonImport(node, source, filePath, edges)
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkPython(node.Child(i), source, filePath, enclosingClass, nodes, edges)
	}
}

func (e *TreeSitterExtractor) processPythonFunction(node *sitter.Node, source []byte, filePath, enclosingClass string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	sig, ret := pySignatureAndReturn(node, source)
	kind := "function"
	if enclosingClass != "" {
		kind = "method"
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind:          kind,
		Name:          name,
		File:          filePath,
		Line:          startLine,
		EndLine:       endLine,
		Body:          node.Content(source),
		Language:      "python",
		QualifiedName: pyQualified(enclosingClass, name),
		Signature:     sig,
		Visibility:    pyVisibility(name),
		IsExported:    pyIsExported(name),
		ReturnType:    ret,
		StartColumn:   int(node.StartPoint().Column),
		EndColumn:     int(node.EndPoint().Column),
	})
	if enclosingClass != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: enclosingClass,
			TargetName: name,
			Kind:       "contains",
			File:       filePath,
			Line:       startLine,
		})
	}
	body := node.ChildByFieldName("body")
	if body == nil {
		body = node
	}
	e.findCallsPython(body, source, filePath, name, edges, make(map[string]bool))
}

func (e *TreeSitterExtractor) processPythonClass(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode) string {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	*nodes = append(*nodes, ExtractedNode{
		Kind:          "class",
		Name:          name,
		File:          filePath,
		Line:          startLine,
		EndLine:       endLine,
		Body:          node.Content(source),
		Language:      "python",
		QualifiedName: name,
		Visibility:    pyVisibility(name),
		IsExported:    pyIsExported(name),
		StartColumn:   int(node.StartPoint().Column),
		EndColumn:     int(node.EndPoint().Column),
	})
	return name
}

func (e *TreeSitterExtractor) findCallsPython(node *sitter.Node, source []byte, filePath string, funcName string, edges *[]ExtractedEdge, seen map[string]bool) {
	// Stop at nested function definitions (named functions get their own nodes via walkPython).
	if node.Type() == "function_definition" {
		return
	}

	if node.Type() == "call" {
		funcNode := node.ChildByFieldName("function")
		if funcNode == nil {
			funcNode = node.Child(0)
		}
		if funcNode != nil {
			calleeName := ""
			if funcNode.Type() == "identifier" {
				calleeName = funcNode.Content(source)
			} else if funcNode.Type() == "attribute" {
				attrNode := funcNode.ChildByFieldName("attribute")
				if attrNode != nil {
					calleeName = attrNode.Content(source)
				}
			}

			if calleeName != "" && calleeName != funcName && !isPythonKeyword(calleeName) {
				callLine := int(node.StartPoint().Row) + 1
				key := fmt.Sprintf("%s:%d", calleeName, callLine)
				if !seen[key] {
					seen[key] = true
					*edges = append(*edges, ExtractedEdge{
						SourceName: funcName,
						TargetName: calleeName,
						Kind:       "calls",
						File:       filePath,
						Line:       callLine,
					})
				}
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.findCallsPython(child, source, filePath, funcName, edges, seen)
	}
}

func (e *TreeSitterExtractor) processPythonImport(node *sitter.Node, source []byte, filePath string, edges *[]ExtractedEdge) {
	// For import_statement: import X
	// For import_from_statement: from X import Y
	nodeType := node.Type()

	if nodeType == "import_statement" {
		// import X
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "dotted_name" {
				importPath := child.Content(source)
				*edges = append(*edges, ExtractedEdge{
					SourceName: filePath,
					TargetName: importPath,
					Kind:       "imports",
					File:       filePath,
					Line:       int(node.StartPoint().Row) + 1,
				})
			}
		}
	} else if nodeType == "import_from_statement" {
		// from X import Y
		moduleNode := node.ChildByFieldName("module_name")
		if moduleNode != nil {
			importPath := moduleNode.Content(source)
			*edges = append(*edges, ExtractedEdge{
				SourceName: filePath,
				TargetName: importPath,
				Kind:       "imports",
				File:       filePath,
				Line:       int(node.StartPoint().Row) + 1,
			})
		}
	}
}

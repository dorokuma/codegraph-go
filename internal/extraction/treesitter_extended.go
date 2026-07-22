package extraction

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// initExtendedLanguages registers additional language grammars into the
// tree-sitter extractor factory. The actual grammar imports live in treesitter.go
// to avoid duplicate import paths.

// initExtendedLanguages registers additional language grammars into the
// tree-sitter extractor factory. The actual grammar imports live in treesitter.go
// to avoid duplicate import paths.
//
//nolint:unused // called via init() registration pattern
func init() {
	// Languages are registered via tsLangRegistry in treesitter.go.
	// This init ensures the extended extractors (extractCLike, extractRust, etc.)
	// are compiled in.
}

// extractCLike handles C, C++, Java, Kotlin, C# — languages with similar
// function/class/method structures. Uses a generic AST walk.
func (e *TreeSitterExtractor) extractCLike(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	e.walkCLike(root, source, filePath, &nodes, &edges, "")
	return nodes, edges
}

func (e *TreeSitterExtractor) walkCLike(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingClass string) {
	switch node.Type() {
	// Functions / methods
	case "function_definition", "function_item", "method_declaration",
		"function_declaration", "constructor_declaration", "constructor_definition",
		"destructor_definition":
		e.processCLikeFunction(node, source, filePath, nodes, edges, enclosingClass)
		return // don't recurse into body
	// Classes / structs / interfaces / traits
	case "class_declaration", "class_definition", "struct_specifier",
		"struct_item", "interface_declaration", "trait_item", "enum_declaration",
		"enum_item", "protocol_declaration":
		className := e.processCLikeClass(node, source, filePath, nodes)
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkCLike(body.Child(i), source, filePath, nodes, edges, className)
			}
		}
		return
	// Imports
	case "preproc_include", "import_declaration", "use_declaration",
		"include_statement", "require_statement", "import_from_statement":
		e.processCLikeImport(node, source, filePath, edges)
		return
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkCLike(node.Child(i), source, filePath, nodes, edges, enclosingClass)
	}
}

func (e *TreeSitterExtractor) processCLikeFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingClass string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		// Some languages use "declarator" for C/C++
		nameNode = node.ChildByFieldName("declarator")
		if nameNode != nil {
			// Walk declarator to find the actual name
			nameNode = findDeepestName(nameNode, source)
		}
	}
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	if name == "" || isBuiltinKeyword(name) {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	kind := "function"
	if enclosingClass != "" {
		kind = "method"
	}

	// Detect constructors
	nodeType := node.Type()
	if strings.Contains(nodeType, "constructor") {
		kind = "constructor"
	}

	sig := extractSimpleSignature(node, source)
	qn := name
	if enclosingClass != "" {
		qn = enclosingClass + "." + name
	}

	exported := isExportedCLike(node, source)
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
		Body:          "",
		Language:      e.language,
		QualifiedName: qn,
		Signature:     sig,
		Visibility:    vis,
		IsExported:    exported,
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

	// Extract calls from function body
	body := node.ChildByFieldName("body")
	if body != nil {
		e.findCLikeCalls(body, source, filePath, name, startLine, edges, make(map[string]bool))
	}
}

func (e *TreeSitterExtractor) processCLikeClass(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode) string {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	name := nameNode.Content(source)
	if name == "" {
		return ""
	}

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	kind := "class"

	nodeType := node.Type()
	switch {
	case strings.Contains(nodeType, "struct"):
		kind = "struct"
	case strings.Contains(nodeType, "interface"):
		kind = "interface"
	case strings.Contains(nodeType, "trait"):
		kind = "trait"
	case strings.Contains(nodeType, "enum"):
		kind = "enum"
	case strings.Contains(nodeType, "protocol"):
		kind = "protocol"
	}

	exported := isExportedCLike(node, source)
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
		Body:          "",
		Language:      e.language,
		QualifiedName: name,
		Visibility:    vis,
		IsExported:    exported,
		StartColumn:   int(node.StartPoint().Column),
		EndColumn:     int(node.EndPoint().Column),
	})

	return name
}

func (e *TreeSitterExtractor) processCLikeImport(node *sitter.Node, source []byte, filePath string, edges *[]ExtractedEdge) {
	line := int(node.StartPoint().Row) + 1

	switch node.Type() {
	case "preproc_include":
		// C/C++: #include <foo.h> or #include "foo.h"
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "string_literal" || child.Type() == "system_lib_string" {
				path := strings.Trim(child.Content(source), "<>\"")
				if path != "" {
					*edges = append(*edges, ExtractedEdge{
						SourceName: filePath, TargetName: path, Kind: "imports", File: filePath, Line: line,
					})
				}
			}
		}
	case "import_declaration":
		// Java/Kotlin: import foo.bar.Baz
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "scoped_identifier" || child.Type() == "identifier" ||
				child.Type() == "dotted_identifier" {
				path := child.Content(source)
				if path != "" {
					*edges = append(*edges, ExtractedEdge{
						SourceName: filePath, TargetName: path, Kind: "imports", File: filePath, Line: line,
					})
				}
			}
		}
	case "use_declaration":
		// Rust: use foo::bar
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "scoped_identifier" || child.Type() == "identifier" {
				path := child.Content(source)
				if path != "" {
					*edges = append(*edges, ExtractedEdge{
						SourceName: filePath, TargetName: path, Kind: "imports", File: filePath, Line: line,
					})
				}
			}
		}
	case "include_statement":
		// Ruby: require 'foo'
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "string" || child.Type() == "simple_symbol" {
				path := strings.Trim(child.Content(source), "'\"")
				if path != "" {
					*edges = append(*edges, ExtractedEdge{
						SourceName: filePath, TargetName: path, Kind: "imports", File: filePath, Line: line,
					})
				}
			}
		}
	case "require_statement", "import_from_statement":
		// PHP: use Foo\Bar  or Python: from X import Y
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "namespace_name" || child.Type() == "dotted_name" ||
				child.Type() == "module_name" {
				path := child.Content(source)
				if path != "" {
					*edges = append(*edges, ExtractedEdge{
						SourceName: filePath, TargetName: path, Kind: "imports", File: filePath, Line: line,
					})
				}
			}
		}
	}
}

func (e *TreeSitterExtractor) findCLikeCalls(node *sitter.Node, source []byte, filePath string, funcName string, funcLine int, edges *[]ExtractedEdge, seen map[string]bool) {
	if node.Type() == "function_declaration" || node.Type() == "method_declaration" || node.Type() == "lambda_expression" {
		return
	}
	// Named function_definition (has its own node via walkLua/extractCLike) gets its
	// own call extraction; only traverse anonymous function_definition bodies (e.g. Lua callbacks).
	if node.Type() == "function_definition" && node.ChildByFieldName("name") != nil {
		return
	}

	if node.Type() == "call_expression" || node.Type() == "call" ||
		node.Type() == "method_invocation" || node.Type() == "navigation_expression" {
		calleeName := extractCalleeName(node, source)
		if calleeName != "" && calleeName != funcName && !seen[calleeName] && !isBuiltinKeyword(calleeName) {
			seen[calleeName] = true
			*edges = append(*edges, ExtractedEdge{
				SourceName: funcName, TargetName: calleeName, Kind: "calls", File: filePath, Line: funcLine,
			})
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		e.findCLikeCalls(node.Child(i), source, filePath, funcName, funcLine, edges, seen)
	}
}

// ---------- Rust extraction ----------

func (e *TreeSitterExtractor) extractRust(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	e.walkRust(root, source, filePath, &nodes, &edges, "")
	return nodes, edges
}

func (e *TreeSitterExtractor) walkRust(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingImpl string) {
	switch node.Type() {
	case "function_item", "function_signature_item":
		e.processRustFunction(node, source, filePath, nodes, edges, enclosingImpl)
		return
	case "impl_item":
		typeNode := node.ChildByFieldName("type")
		implName := ""
		if typeNode != nil {
			implName = typeNode.Content(source)
		}
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkRust(body.Child(i), source, filePath, nodes, edges, implName)
			}
		}
		return
	case "struct_item", "enum_item", "trait_item":
		e.processRustType(node, source, filePath, nodes)
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkRust(body.Child(i), source, filePath, nodes, edges, "")
			}
		}
		return
	case "use_declaration":
		e.processCLikeImport(node, source, filePath, edges)
		return
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkRust(node.Child(i), source, filePath, nodes, edges, enclosingImpl)
	}
}

func (e *TreeSitterExtractor) processRustFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingImpl string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	kind := "function"
	qn := name
	if enclosingImpl != "" {
		kind = "method"
		qn = enclosingImpl + "::" + name
	}
	sig := extractSimpleSignature(node, source)
	exported := false
	for i := 0; i < int(node.ChildCount()); i++ {
		if node.Child(i).Type() == "visibility_modifier" {
			exported = strings.Contains(node.Child(i).Content(source), "pub")
			break
		}
	}
	vis := ""
	if exported {
		vis = "public"
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
		Language: "rust", QualifiedName: qn, Signature: sig,
		Visibility: vis, IsExported: exported,
		StartColumn: int(node.StartPoint().Column), EndColumn: int(node.EndPoint().Column),
	})
	if enclosingImpl != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: enclosingImpl, TargetName: name, Kind: "contains", File: filePath, Line: startLine,
		})
	}
	body := node.ChildByFieldName("body")
	if body != nil {
		e.findCLikeCalls(body, source, filePath, name, startLine, edges, make(map[string]bool))
	}
}

func (e *TreeSitterExtractor) processRustType(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	kind := "struct"
	switch node.Type() {
	case "enum_item":
		kind = "enum"
	case "trait_item":
		kind = "trait"
	}
	exported := false
	for i := 0; i < int(node.ChildCount()); i++ {
		if node.Child(i).Type() == "visibility_modifier" {
			exported = strings.Contains(node.Child(i).Content(source), "pub")
			break
		}
	}
	vis := ""
	if exported {
		vis = "public"
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
		Language: "rust", QualifiedName: name, Visibility: vis, IsExported: exported,
		StartColumn: int(node.StartPoint().Column), EndColumn: int(node.EndPoint().Column),
	})
}

// ---------- Ruby extraction ----------

func (e *TreeSitterExtractor) extractRuby(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	e.walkRuby(root, source, filePath, &nodes, &edges, "")
	return nodes, edges
}

func (e *TreeSitterExtractor) walkRuby(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingClass string) {
	switch node.Type() {
	case "method", "singleton_method":
		e.processRubyMethod(node, source, filePath, nodes, edges, enclosingClass)
		return
	case "class", "module":
		nameNode := node.ChildByFieldName("name")
		name := ""
		if nameNode != nil {
			name = nameNode.Content(source)
		}
		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1
		kind := "class"
		if node.Type() == "module" {
			kind = "module"
		}
		*nodes = append(*nodes, ExtractedNode{
			Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
			Language: "ruby", QualifiedName: name, Visibility: "public", IsExported: true,
			StartColumn: int(node.StartPoint().Column), EndColumn: int(node.EndPoint().Column),
		})
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkRuby(body.Child(i), source, filePath, nodes, edges, name)
			}
		}
		return
	case "call":
		// require 'foo'
		methodNode := node.ChildByFieldName("method")
		if methodNode != nil && (methodNode.Content(source) == "require" || methodNode.Content(source) == "require_relative") {
			argNode := node.ChildByFieldName("arguments")
			if argNode != nil {
				for i := 0; i < int(argNode.ChildCount()); i++ {
					ch := argNode.Child(i)
					if ch.Type() == "string" || ch.Type() == "simple_symbol" {
						path := strings.Trim(ch.Content(source), "'\"")
						if path != "" {
							*edges = append(*edges, ExtractedEdge{
								SourceName: filePath, TargetName: path, Kind: "imports", File: filePath,
								Line: int(node.StartPoint().Row) + 1,
							})
						}
					}
				}
			}
		}
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkRuby(node.Child(i), source, filePath, nodes, edges, enclosingClass)
	}
}

func (e *TreeSitterExtractor) processRubyMethod(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingClass string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	kind := "function"
	qn := name
	if enclosingClass != "" {
		kind = "method"
		qn = enclosingClass + "." + name
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
		Language: "ruby", QualifiedName: qn,
		Visibility: "public", IsExported: !strings.HasPrefix(name, "_"),
		StartColumn: int(node.StartPoint().Column), EndColumn: int(node.EndPoint().Column),
	})
	if enclosingClass != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: enclosingClass, TargetName: name, Kind: "contains", File: filePath, Line: startLine,
		})
	}
	body := node.ChildByFieldName("body")
	if body != nil {
		e.findCLikeCalls(body, source, filePath, name, startLine, edges, make(map[string]bool))
	}
}

// ---------- PHP extraction ----------

func (e *TreeSitterExtractor) extractPHP(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	e.walkPHP(root, source, filePath, &nodes, &edges, "")
	return nodes, edges
}

func (e *TreeSitterExtractor) walkPHP(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingClass string) {
	switch node.Type() {
	case "function_definition":
		e.processPHPFunction(node, source, filePath, nodes, edges, enclosingClass)
		return
	case "method_declaration":
		e.processPHPFunction(node, source, filePath, nodes, edges, enclosingClass)
		return
	case "class_declaration", "interface_declaration", "trait_declaration":
		nameNode := node.ChildByFieldName("name")
		name := ""
		if nameNode != nil {
			name = nameNode.Content(source)
		}
		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1
		kind := "class"
		if strings.Contains(node.Type(), "interface") {
			kind = "interface"
		} else if strings.Contains(node.Type(), "trait") {
			kind = "trait"
		}
		*nodes = append(*nodes, ExtractedNode{
			Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
			Language: "php", QualifiedName: name, Visibility: "public", IsExported: true,
			StartColumn: int(node.StartPoint().Column), EndColumn: int(node.EndPoint().Column),
		})
		body := node.ChildByFieldName("body")
		if body != nil {
			for i := 0; i < int(body.ChildCount()); i++ {
				e.walkPHP(body.Child(i), source, filePath, nodes, edges, name)
			}
		}
		return
	case "namespace_use_declaration", "use_declaration":
		e.processCLikeImport(node, source, filePath, edges)
		return
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkPHP(node.Child(i), source, filePath, nodes, edges, enclosingClass)
	}
}

func (e *TreeSitterExtractor) processPHPFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingClass string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	kind := "function"
	qn := name
	if enclosingClass != "" {
		kind = "method"
		qn = enclosingClass + "." + name
	}
	sig := extractSimpleSignature(node, source)
	vis := ""
	exported := true
	// Check visibility modifier
	for i := 0; i < int(node.ChildCount()); i++ {
		ch := node.Child(i)
		if ch.Type() == "visibility_modifier" {
			v := ch.Content(source)
			vis = v
			if v == "private" || v == "protected" {
				exported = false
			}
			break
		}
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
		Language: "php", QualifiedName: qn, Signature: sig,
		Visibility: vis, IsExported: exported,
		StartColumn: int(node.StartPoint().Column), EndColumn: int(node.EndPoint().Column),
	})
	if enclosingClass != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: enclosingClass, TargetName: name, Kind: "contains", File: filePath, Line: startLine,
		})
	}
	body := node.ChildByFieldName("body")
	if body != nil {
		e.findCLikeCalls(body, source, filePath, name, startLine, edges, make(map[string]bool))
	}
}

// ---------- Helpers ----------

func findDeepestName(node *sitter.Node, source []byte) *sitter.Node {
	if node == nil {
		return nil
	}
	if node.Type() == "identifier" || node.Type() == "field_identifier" ||
		node.Type() == "type_identifier" {
		return node
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		if found := findDeepestName(node.Child(i), source); found != nil {
			return found
		}
	}
	return node
}

func extractSimpleSignature(node *sitter.Node, source []byte) string {
	params := node.ChildByFieldName("parameters")
	if params != nil {
		return params.Content(source)
	}
	return ""
}

func extractCalleeName(node *sitter.Node, source []byte) string {
	// Try "function" field (most languages)
	fn := node.ChildByFieldName("function")
	if fn != nil {
		if fn.Type() == "identifier" || fn.Type() == "field_identifier" {
			return fn.Content(source)
		}
		if fn.Type() == "scoped_identifier" || fn.Type() == "member_expression" ||
			fn.Type() == "navigation_expression" {
			// Get last segment
			field := fn.ChildByFieldName("field")
			if field != nil {
				return field.Content(source)
			}
			// Try last child
			last := fn.Child(int(fn.ChildCount()) - 1)
			if last != nil {
				return last.Content(source)
			}
		}
	}
	// Try "method" field (Java method_invocation)
	method := node.ChildByFieldName("method")
	if method != nil {
		return method.Content(source)
	}
	// Try "name" field
	name := node.ChildByFieldName("name")
	if name != nil {
		return name.Content(source)
	}
	return ""
}

func isExportedCLike(node *sitter.Node, source []byte) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		ch := node.Child(i)
		if ch.Type() == "visibility_modifier" {
			v := ch.Content(source)
			return strings.Contains(v, "pub") || strings.Contains(v, "public")
		}
		// C++: check for "export" keyword
		if ch.Type() == "storage_class_specifier" && ch.Content(source) == "export" {
			return true
		}
	}
	return false
}

func isBuiltinKeyword(s string) bool {
	switch s {
	case "if", "else", "for", "while", "do", "switch", "case", "default",
		"break", "continue", "return", "throw", "try", "catch", "finally",
		"new", "delete", "typeof", "instanceof", "void",
		"this", "super", "import", "export", "from",
		"async", "await", "yield", "defer", "go", "goto",
		"null", "undefined", "true", "false", "nil", "None", "self", "Self",
		"var", "let", "const", "function", "class", "struct", "interface",
		"enum", "trait", "impl", "mod", "use", "fn", "pub", "priv",
		"static", "final", "abstract", "virtual", "override",
		"println", "print", "assert", "panic", "require", "include":
		return true
	}
	return false
}

// ---------- Lua extraction ----------

func (e *TreeSitterExtractor) extractLua(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge
	e.walkLua(root, source, filePath, &nodes, &edges, "")
	return nodes, edges
}

func (e *TreeSitterExtractor) walkLua(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingTable string) {
	switch node.Type() {
	case "function_declaration", "local_function":
		e.processLuaFunction(node, source, filePath, nodes, edges, enclosingTable)
		return
	case "function_definition":
		// May be anonymous (M.foo = function() … end) or named.
		e.processLuaFunction(node, source, filePath, nodes, edges, enclosingTable)
		return
	case "assignment_statement", "local_variable_declaration":
		// Assignment-style functions: M.foo = function() … end
		e.processLuaAssignment(node, source, filePath, nodes, edges, enclosingTable)
		return
	case "table_constructor":
		// Could be a module table
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		e.walkLua(node.Child(i), source, filePath, nodes, edges, enclosingTable)
	}
}

func (e *TreeSitterExtractor) processLuaFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingTable string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		// Try declarator for assigned functions
		nameNode = node.ChildByFieldName("declarator")
	}
	if nameNode == nil {
		// Anonymous function_definition without assignment context (e.g. callback arg).
		// Skip — the parent assignment_statement handler will pick it up if relevant.
		return
	}
	name := nameNode.Content(source)
	if name == "" {
		return
	}
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	kind := "function"
	qn := name
	if enclosingTable != "" {
		kind = "method"
		qn = enclosingTable + "." + name
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
		Language: "lua", QualifiedName: qn,
		Visibility: "public", IsExported: !strings.HasPrefix(name, "_"),
		StartColumn: int(node.StartPoint().Column), EndColumn: int(node.EndPoint().Column),
	})
	if enclosingTable != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: enclosingTable, TargetName: name, Kind: "contains", File: filePath, Line: startLine,
		})
	}
	body := node.ChildByFieldName("body")
	if body != nil {
		e.findCLikeCalls(body, source, filePath, name, startLine, edges, make(map[string]bool))
	}
}

// processLuaAssignment handles M.foo = function() … end style definitions.
// The assignment node contains a variable_list on the left and an expression_list
// with an anonymous function_definition on the right.
func (e *TreeSitterExtractor) processLuaAssignment(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingTable string) {
	// Extract variable name from left-hand side.
	var lhsName string
	for i := 0; i < int(node.ChildCount()); i++ {
		ch := node.Child(i)
		switch ch.Type() {
		case "variable_list", "variable_declarator":
			lhsName = luaExtractVariableName(ch, source)
		}
	}
	if lhsName == "" {
		return
	}
	// Find the function_definition child and emit a node for it.
	for i := 0; i < int(node.ChildCount()); i++ {
		ch := node.Child(i)
		if ch.Type() == "function_definition" {
			e.emitLuaAnonFunc(ch, source, filePath, nodes, edges, enclosingTable, lhsName)
			return
		}
		if ch.Type() == "expression_list" {
			for j := 0; j < int(ch.ChildCount()); j++ {
				gc := ch.Child(j)
				if gc.Type() == "function_definition" {
					e.emitLuaAnonFunc(gc, source, filePath, nodes, edges, enclosingTable, lhsName)
					return
				}
			}
		}
	}
}

// luaExtractVariableName extracts a name from a Lua variable expression.
// Handles dot_index_expression (M.foo), bracket_index_expression (M["foo"]),
// and plain identifier.
func luaExtractVariableName(node *sitter.Node, source []byte) string {
	switch node.Type() {
	case "identifier":
		return node.Content(source)
	case "dot_index_expression":
		// table.field → return field name
		for i := 0; i < int(node.ChildCount()); i++ {
			ch := node.Child(i)
			if ch.Type() == "identifier" || ch.Type() == "property_identifier" {
				return ch.Content(source)
			}
		}
		// Fallback: full expression
		return node.Content(source)
	case "bracket_index_expression":
		// table["field"] → return string content or full expression
		for i := 0; i < int(node.ChildCount()); i++ {
			ch := node.Child(i)
			if ch.Type() == "string" {
				s := ch.Content(source)
				return strings.Trim(s, `"'`)
			}
		}
		return node.Content(source)
	default:
		// For variable_list, walk children to find first named child.
		for i := 0; i < int(node.ChildCount()); i++ {
			if name := luaExtractVariableName(node.Child(i), source); name != "" {
				return name
			}
		}
		return ""
	}
}

// emitLuaAnonFunc creates a node for an anonymous function that was assigned to a variable.
func (e *TreeSitterExtractor) emitLuaAnonFunc(fn *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge, enclosingTable, name string) {
	startLine := int(fn.StartPoint().Row) + 1
	endLine := int(fn.EndPoint().Row) + 1
	kind := "function"
	qn := name
	if enclosingTable != "" {
		kind = "method"
		qn = enclosingTable + "." + name
	}
	*nodes = append(*nodes, ExtractedNode{
		Kind: kind, Name: name, File: filePath, Line: startLine, EndLine: endLine,
		Language: "lua", QualifiedName: qn,
		Visibility: "public", IsExported: !strings.HasPrefix(name, "_"),
		StartColumn: int(fn.StartPoint().Column), EndColumn: int(fn.EndPoint().Column),
	})
	if enclosingTable != "" {
		*edges = append(*edges, ExtractedEdge{
			SourceName: enclosingTable, TargetName: name, Kind: "contains", File: filePath, Line: startLine,
		})
	}
	body := fn.ChildByFieldName("body")
	if body != nil {
		e.findCLikeCalls(body, source, filePath, name, startLine, edges, make(map[string]bool))
	}
}

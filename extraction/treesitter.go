package extraction

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	golang "github.com/smacker/go-tree-sitter/golang"
	python "github.com/smacker/go-tree-sitter/python"
	typescript "github.com/smacker/go-tree-sitter/typescript/tsx"
)

// TreeSitterExtractor uses tree-sitter for AST-based extraction.
type TreeSitterExtractor struct {
	language string
	parser   *sitter.Parser
	lang     *sitter.Language
}

// NewTreeSitterExtractor creates a tree-sitter extractor for the given language.
func NewTreeSitterExtractor(language string) *TreeSitterExtractor {
	var lang *sitter.Language

	switch language {
	case "go":
		lang = golang.GetLanguage()
	case "typescript", "javascript":
		lang = typescript.GetLanguage()
	case "python":
		lang = python.GetLanguage()
	default:
		return nil // unsupported
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	return &TreeSitterExtractor{
		language: language,
		parser:   parser,
		lang:     lang,
	}
}

// Extract parses the source code and returns nodes and edges using tree-sitter.
func (e *TreeSitterExtractor) Extract(source string, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	if e.parser == nil {
		return nil, nil
	}

	sourceBytes := []byte(source)
	tree, err := e.parser.ParseCtx(context.Background(), nil, sourceBytes)
	if err != nil {
		return nil, nil
	}
	defer tree.Close()

	root := tree.RootNode()

	switch e.language {
	case "go":
		return e.extractGo(root, sourceBytes, filePath)
	case "typescript", "javascript":
		return e.extractJS(root, sourceBytes, filePath)
	case "python":
		return e.extractPython(root, sourceBytes, filePath)
	}

	return nil, nil
}

// ---------- Go extraction ----------

func (e *TreeSitterExtractor) extractGo(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	// Walk the AST
	e.walk(root, source, filePath, &nodes, &edges)

	return nodes, edges
}

func (e *TreeSitterExtractor) walk(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nodeType := node.Type()

	switch nodeType {
	case "function_declaration":
		e.processFunction(node, source, filePath, "function", nodes, edges)
	case "method_declaration":
		e.processFunction(node, source, filePath, "method", nodes, edges)
	case "type_declaration":
		e.processType(node, source, filePath, nodes)
	case "import_declaration":
		e.processImport(node, source, filePath, edges)
	}

	// Recurse into children
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.walk(child, source, filePath, nodes, edges)
	}
}

func (e *TreeSitterExtractor) processFunction(node *sitter.Node, source []byte, filePath string, kind string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	// Get function name
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)

	// Get line numbers
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	// Get body
	body := node.Content(source)

	*nodes = append(*nodes, ExtractedNode{
		Kind:     kind,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		EndLine:  endLine,
		Body:     body,
		Language: "go",
	})

	// Extract calls from function body
	e.extractCallsFromNode(node, source, filePath, name, startLine, edges)
}

func (e *TreeSitterExtractor) processType(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode) {
	// Get type name
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)

	// Determine kind (struct or interface)
	kind := "struct"
	typeNode := node.ChildByFieldName("type")
	if typeNode != nil {
		if typeNode.Type() == "interface_type" {
			kind = "interface"
		}
	}

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	*nodes = append(*nodes, ExtractedNode{
		Kind:     kind,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		EndLine:  endLine,
		Body:     node.Content(source),
		Language: "go",
	})
}



func (e *TreeSitterExtractor) extractCallsFromNode(funcNode *sitter.Node, source []byte, filePath string, funcName string, funcLine int, edges *[]ExtractedEdge) {
	// Find call_expression nodes in the function body, but NOT in nested functions
	bodyNode := funcNode.ChildByFieldName("body")
	if bodyNode == nil {
		return
	}
	e.findCalls(bodyNode, source, filePath, funcName, funcLine, edges, make(map[string]bool))
}

func (e *TreeSitterExtractor) findCalls(node *sitter.Node, source []byte, filePath string, funcName string, funcLine int, edges *[]ExtractedEdge, seen map[string]bool) {
	// Stop at nested function declarations - don't attribute their calls to the outer function
	if node.Type() == "function_declaration" || node.Type() == "method_declaration" || node.Type() == "func_literal" {
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

			if calleeName != "" && calleeName != funcName && !seen[calleeName] && !isGoKeyword(calleeName) {
				seen[calleeName] = true
				*edges = append(*edges, ExtractedEdge{
					SourceName: funcName,
					TargetName: calleeName,
					Kind:       "calls",
					File:       filePath,
					Line:       funcLine,
				})
			}
		}
	}

	// Recurse
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.findCalls(child, source, filePath, funcName, funcLine, edges, seen)
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

	e.walkJS(root, source, filePath, &nodes, &edges)

	return nodes, edges
}

func (e *TreeSitterExtractor) walkJS(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nodeType := node.Type()

	switch nodeType {
	case "function_declaration", "arrow_function", "function_expression":
		e.processJSFunction(node, source, filePath, nodes, edges)
		return // Don't recurse into children, processJSFunction handles calls
	case "class_declaration":
		e.processJSClass(node, source, filePath, nodes, edges)
	case "import_statement":
		e.processJSImport(node, source, filePath, edges)
	}

	// Recurse
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.walkJS(child, source, filePath, nodes, edges)
	}
}

func (e *TreeSitterExtractor) processJSFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	*nodes = append(*nodes, ExtractedNode{
		Kind:     "function",
		Name:     name,
		File:     filePath,
		Line:     startLine,
		EndLine:  endLine,
		Body:     node.Content(source),
		Language: e.language,
	})

	// Extract calls
	e.findCallsJS(node, source, filePath, name, startLine, edges, make(map[string]bool))
}

func (e *TreeSitterExtractor) processJSClass(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	*nodes = append(*nodes, ExtractedNode{
		Kind:     "class",
		Name:     name,
		File:     filePath,
		Line:     startLine,
		EndLine:  endLine,
		Body:     node.Content(source),
		Language: e.language,
	})
}

func (e *TreeSitterExtractor) findCallsJS(node *sitter.Node, source []byte, filePath string, funcName string, funcLine int, edges *[]ExtractedEdge, seen map[string]bool) {
	// Stop at nested function declarations
	if node.Type() == "function_declaration" || node.Type() == "arrow_function" || node.Type() == "function_expression" {
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

			if calleeName != "" && calleeName != funcName && !seen[calleeName] && !isJSKeyword(calleeName) {
				seen[calleeName] = true
				*edges = append(*edges, ExtractedEdge{
					SourceName: funcName,
					TargetName: calleeName,
					Kind:       "calls",
					File:       filePath,
					Line:       funcLine,
				})
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.findCallsJS(child, source, filePath, funcName, funcLine, edges, seen)
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

func (e *TreeSitterExtractor) extractPython(root *sitter.Node, source []byte, filePath string) ([]ExtractedNode, []ExtractedEdge) {
	var nodes []ExtractedNode
	var edges []ExtractedEdge

	e.walkPython(root, source, filePath, &nodes, &edges)

	return nodes, edges
}

func (e *TreeSitterExtractor) walkPython(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nodeType := node.Type()

	switch nodeType {
	case "function_definition":
		e.processPythonFunction(node, source, filePath, nodes, edges)
	case "class_definition":
		e.processPythonClass(node, source, filePath, nodes, edges)
	case "import_statement", "import_from_statement":
		e.processPythonImport(node, source, filePath, edges)
	}

	// Recurse
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.walkPython(child, source, filePath, nodes, edges)
	}
}

func (e *TreeSitterExtractor) processPythonFunction(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	*nodes = append(*nodes, ExtractedNode{
		Kind:     "function",
		Name:     name,
		File:     filePath,
		Line:     startLine,
		EndLine:  endLine,
		Body:     node.Content(source),
		Language: "python",
	})

	// Extract calls
	e.findCallsPython(node, source, filePath, name, startLine, edges, make(map[string]bool))
}

func (e *TreeSitterExtractor) processPythonClass(node *sitter.Node, source []byte, filePath string, nodes *[]ExtractedNode, edges *[]ExtractedEdge) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(source)

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	*nodes = append(*nodes, ExtractedNode{
		Kind:     "class",
		Name:     name,
		File:     filePath,
		Line:     startLine,
		EndLine:  endLine,
		Body:     node.Content(source),
		Language: "python",
	})
}

func (e *TreeSitterExtractor) findCallsPython(node *sitter.Node, source []byte, filePath string, funcName string, funcLine int, edges *[]ExtractedEdge, seen map[string]bool) {
	// Stop at nested function definitions
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

			if calleeName != "" && calleeName != funcName && !seen[calleeName] && !isPythonKeyword(calleeName) {
				seen[calleeName] = true
				*edges = append(*edges, ExtractedEdge{
					SourceName: funcName,
					TargetName: calleeName,
					Kind:       "calls",
					File:       filePath,
					Line:       funcLine,
				})
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		e.findCallsPython(child, source, filePath, funcName, funcLine, edges, seen)
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

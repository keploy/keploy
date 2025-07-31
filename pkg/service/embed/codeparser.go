package embed

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
)

var languageExtensionMap = map[string]string{
	"py": "python",
	"js": "javascript",
	"go": "go",
}

// PointOfInterest represents a node and its type label
type PointOfInterest struct {
	Node  *sitter.Node
	Label string
}

// CodeParser holds the tree-sitter languages
type CodeParser struct {
	languages map[string]*sitter.Language
}

func NewCodeParser() (*CodeParser, error) {
	cp := &CodeParser{
		languages: make(map[string]*sitter.Language),
	}
	cp.languages["go"] = golang.GetLanguage()
	cp.languages["python"] = python.GetLanguage()
	cp.languages["javascript"] = javascript.GetLanguage()
	return cp, nil
}

func (cp *CodeParser) ParseCode(code string, fileExtension string) (*sitter.Node, error) {
	languageName, ok := languageExtensionMap[fileExtension]
	if !ok {
		return nil, fmt.Errorf("unsupported file type: %s. Supported extensions: go, py, js", fileExtension)
	}

	lang, ok := cp.languages[languageName]
	if !ok || lang == nil {
		return nil, fmt.Errorf("language parser for %s (extension: %s) not loaded", languageName, fileExtension)
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, []byte(code))
	if err != nil {
		return nil, fmt.Errorf("failed to parse code (ext: %s): %w", fileExtension, err)
	}
	if tree == nil {
		return nil, fmt.Errorf("failed to parse the code (ext: %s), tree is nil", fileExtension)
	}
	return tree.RootNode(), nil
}



var queries = map[string]string{
	"go": `
		(function_declaration) @func
		(method_declaration) @func
	`,
	"python": `
		(function_definition) @func
		(class_definition) @class
	`,
	"javascript": `
		(function_declaration) @func
		(arrow_function) @func
		(method_definition) @func
		(class_declaration) @class
	`,
}

// ExtractPointsOfInterest to find all function and method declarations.
func (cp *CodeParser) ExtractPointsOfInterest(rootNode *sitter.Node, fileExtension string) ([]PointOfInterest, error) {
	lang, ok := cp.languages[languageExtensionMap[fileExtension]]
	if !ok {
		return nil, fmt.Errorf("language not supported for query: %s", fileExtension)
	}

	queryStr, ok := queries[languageExtensionMap[fileExtension]]
	if !ok {
		return nil, fmt.Errorf("no query found for language: %s", languageExtensionMap[fileExtension])
	}

	q, err := sitter.NewQuery([]byte(queryStr), lang)
	if err != nil {
		return nil, fmt.Errorf("failed to create query for %s: %w", fileExtension, err)
	}

	qc := sitter.NewQueryCursor()
	qc.Exec(q, rootNode)

	var points []PointOfInterest
	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			var label string
			captureName := q.CaptureNameForId(c.Index)
			switch captureName {
			case "func":
				// For Go, we need to differentiate between a function and a method.
				if fileExtension == "go" && c.Node.Type() == "method_declaration" {
					label = "Method"
				} else {
					label = "Function"
				}
			case "class":
				label = "Class"
			default:
				// Fallback for any other captures, though we don't expect any.
				label = captureName
			}
			points = append(points, PointOfInterest{Node: c.Node, Label: label})
		}
	}

	return points, nil
}

var nodesForCommentsData = map[string]map[string]string{
	"py": {
		"comment":   "Comment",
		"decorator": "Decorator",
	},
	"js": {
		"comment":   "Comment",
		"decorator": "Decorator",
	},
	"go": {
		"comment": "Comment",
	},
}

func (cp *CodeParser) getNodesForComments(fileExtension string) (map[string]string, error) {
	types, ok := nodesForCommentsData[fileExtension]
	if !ok {
		return nil, fmt.Errorf("unsupported file type for comments: %s. Supported: py, js, go", fileExtension)
	}
	return types, nil
}

// extracts comments and decorators from the code.
func (cp *CodeParser) ExtractComments(node *sitter.Node, fileExtension string) ([]PointOfInterest, error) {
	commentMap, err := cp.getNodesForComments(fileExtension)
	if err != nil {
		return nil, err
	}

	var comments []PointOfInterest
	var recurse func(n *sitter.Node)
	recurse = func(n *sitter.Node) {
		if n == nil {
			return
		}
		nodeType := n.Type()
		if label, ok := commentMap[nodeType]; ok {
			comments = append(comments, PointOfInterest{Node: n, Label: label})
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			recurse(n.Child(i))
		}
	}
	recurse(node)
	return comments, nil
}

// Extracts the name of a symbol, e.g., function name, class name.
func (cp *CodeParser) ExtractSymbolName(node *sitter.Node, code []byte) string {
	var nameNode *sitter.Node
	nodeType := node.Type()
	switch nodeType {
	case "function_definition", "class_definition": // Python
		nameNode = node.ChildByFieldName("name")
	case "function_declaration", "class_declaration": // JavaScript
		nameNode = node.ChildByFieldName("name")
	case "method_declaration": // Go
		nameNode = node.ChildByFieldName("name")
	case "type_declaration":
		typeSpecNode := node.ChildByFieldName("type")
		if typeSpecNode != nil {
			nameNode = node.ChildByFieldName("name")
		}
	}

	if nameNode != nil {
		return nameNode.Content(code)
	}

	// Fallback for Go function declarations where the name is directly a child
	if nodeType == "function_declaration" {
		nameNode = node.ChildByFieldName("name")
		if nameNode != nil {
			return nameNode.Content(code)
		}
	}
	return ""
}

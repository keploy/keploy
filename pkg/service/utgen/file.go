package utgen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"strings"
)

// FilePreprocessor processes files based on specified rules
type FilePreprocessor struct {
	PathToFile string
	Rules      []Rule
}

// Rule defines a condition-action pair
type Rule struct {
	Condition func() bool
	Action    func(string) string
}

// NewFilePreprocessor initializes a new FilePreprocessor
func NewFilePreprocessor(pathToFile string) *FilePreprocessor {
	fp := &FilePreprocessor{PathToFile: pathToFile}
	fp.Rules = []Rule{
		{fp.isPythonFile, fp.processIfPython},
	}
	return fp
}

// ProcessFile processes the text based on internal rules
func (fp *FilePreprocessor) ProcessFile(text string) string {
	for _, rule := range fp.Rules {
		if rule.Condition() {
			return rule.Action(text)
		}
	}
	return text // Return the text unchanged if no rules apply
}

func (fp *FilePreprocessor) isPythonFile() bool {
	return strings.HasSuffix(fp.PathToFile, ".py")
}

func (fp *FilePreprocessor) processIfPython(text string) string {
	if fp.containsClassDefinition() {
		return indent(text, "    ")
	}
	return text
}

func (fp *FilePreprocessor) containsClassDefinition() bool {
	content, err := ioutil.ReadFile(fp.PathToFile)
	if err != nil {
		fmt.Printf("Error reading file: %v\n", err)
		return false
	}

	fs := token.NewFileSet()
	node, err := parser.ParseFile(fs, fp.PathToFile, content, parser.AllErrors)
	if err != nil {
		fmt.Printf("Syntax error when parsing the file: %v\n", err)
		return false
	}

	for _, decl := range node.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok {
			for _, spec := range genDecl.Specs {
				if _, ok := spec.(*ast.TypeSpec); ok {
					return true
				}
			}
		}
	}
	return false
}

func indent(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

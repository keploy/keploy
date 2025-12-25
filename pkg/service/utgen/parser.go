package utgen

import (
	"fmt"
	"strings"
)

type Language string

const (
	Python      Language = "python"
	Go          Language = "go"
	JavaScript  Language = "javascript"
	TypeScript  Language = "typescript"
	Java        Language = "java"
	CSharp      Language = "csharp"
	CPlusPlus   Language = "cplusplus"
	Ruby        Language = "ruby"
	Rust        Language = "rust"
)

type CodeParser struct{}

func NewCodeParser() *CodeParser {
	return &CodeParser{}
}

func (p *CodeParser) ParseLanguage(filepath string) (Language, error) {
	ext := strings.ToLower(getFileExtension(filepath))
	
	switch ext {
	case ".py":
		return Python, nil
	case ".go":
		return Go, nil
	case ".js":
		return JavaScript, nil
	case ".ts":
		return TypeScript, nil
	case ".java":
		return Java, nil
	case ".cs":
		return CSharp, nil
	case ".cpp", ".cc", ".cxx", ".h":
		return CPlusPlus, nil
	case ".rb":
		return Ruby, nil
	case ".rs":
		return Rust, nil
	default:
		return "", fmt.Errorf("unsupported extension: %s", ext)
	}
}

func (p *CodeParser) GetIndentation(content string, lang Language) (int, error) {
	analyzer := NewIndentationAnalyzer()
	return analyzer.Analyze(content, lang)
}

func (p *CodeParser) FindInsertionPoint(content string, lang Language) (int, error) {
	finder := NewInsertionPointFinder()
	return finder.Find(content, lang)
}

// getFileExtension extracts the file extension from a filepath string

// getFileExtension extracts the file extension from a filepath string

// getFileExtension extracts the file extension from a filepath string
func getFileExtension(filepath string) string {
	lastDot := strings.LastIndex(filepath, ".")
	if lastDot == -1 {
		return ""
	}
	return filepath[lastDot:]
}

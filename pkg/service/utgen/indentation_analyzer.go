package utgen

import (
	"fmt"
	"strings"
)

type IndentationAnalyzer struct {
	cache map[string]int
}

func NewIndentationAnalyzer() *IndentationAnalyzer {
	return &IndentationAnalyzer{
		cache: make(map[string]int),
	}
}

func (ia *IndentationAnalyzer) Analyze(content string, language Language) (int, error) {
	if content == "" {
		return 4, fmt.Errorf("empty content")
	}

	lines := strings.Split(content, "\n")
	
	switch language {
	case Python:
		return ia.analyzePython(lines)
	case Go:
		return ia.analyzeGo(lines)
	case JavaScript, TypeScript:
		return ia.analyzeJavaScript(lines)
	case Java, CSharp:
		return ia.analyzeJava(lines)
	case CPlusPlus:
		return ia.analyzeCPlusPlus(lines)
	case Ruby:
		return ia.analyzeRuby(lines)
	case Rust:
		return ia.analyzeRust(lines)
	default:
		return 4, fmt.Errorf("unsupported language: %v", language)
	}
}

func (ia *IndentationAnalyzer) analyzePython(lines []string) (int, error) {
	var testIndents []int
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "def test_") {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			testIndents = append(testIndents, indent)
		}
	}
	if len(testIndents) > 0 {
		return testIndents[len(testIndents)-1], nil
	}
	return 4, nil
}

func (ia *IndentationAnalyzer) analyzeGo(lines []string) (int, error) {
	return 2, nil // Go standard
}

func (ia *IndentationAnalyzer) analyzeJavaScript(lines []string) (int, error) {
	return 2, nil // JS standard
}

func (ia *IndentationAnalyzer) analyzeJava(lines []string) (int, error) {
	return 4, nil // Java standard
}

func (ia *IndentationAnalyzer) analyzeCPlusPlus(lines []string) (int, error) {
	return 4, nil
}

func (ia *IndentationAnalyzer) analyzeRuby(lines []string) (int, error) {
	return 2, nil
}

func (ia *IndentationAnalyzer) analyzeRust(lines []string) (int, error) {
	return 4, nil
}

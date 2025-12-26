package utgen

import (
	"strings"
)

type InsertionPointFinder struct{}

func NewInsertionPointFinder() *InsertionPointFinder {
	return &InsertionPointFinder{}
}

func (ipf *InsertionPointFinder) Find(content string, language Language) (int, error) {
	lines := strings.Split(content, "\n")
	
	// Find last test function and return line after it
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		switch language {
		case Python:
			if strings.HasPrefix(line, "def test_") {
				return i + 2, nil // After function + blank line
			}
		case Go:
			if strings.HasPrefix(line, "func Test") {
				return i + 2, nil
			}
		case JavaScript, TypeScript:
			if strings.Contains(line, "it('") || strings.Contains(line, "test(") {
				return i + 2, nil
			}
		case Java:
			if strings.Contains(line, "@Test") || strings.Contains(line, "test") {
				return i + 2, nil
			}
		}
	}
	
	// Fallback: end of file
	return len(lines), nil
}

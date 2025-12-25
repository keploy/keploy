package utgen

import (
	"testing"
)

func TestParserBasic(t *testing.T) {
	parser := NewCodeParser()
	
	lang, err := parser.ParseLanguage("test.py")
	if err != nil || lang != Python {
		t.Errorf("Expected Python for .py, got %v", lang)
	}
	
	lang, err = parser.ParseLanguage("test.go")
	if err != nil || lang != Go {
		t.Errorf("Expected Go for .go, got %v", lang)
	}
}

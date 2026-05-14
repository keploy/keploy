package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHighlightFunctions_AnsiDisabled_3829 verifies that all Highlight*
// functions return clean text (no brackets) when ANSI output is disabled.
func TestHighlightFunctions_AnsiDisabled_3829(t *testing.T) {
	// Save original value and restore after tests.
	origDisabled := IsAnsiDisabled
	defer func() { IsAnsiDisabled = origDisabled }()

	IsAnsiDisabled = true

	t.Run("HighlightString_SingleArg", func(t *testing.T) {
		result := HighlightString("test-1")
		assert.Equal(t, "test-1", result)
	})

	t.Run("HighlightString_MultipleArgs", func(t *testing.T) {
		result := HighlightString("hello", " ", "world")
		assert.Equal(t, "hello world", result)
	})

	t.Run("HighlightString_NumericArg", func(t *testing.T) {
		result := HighlightString(85.5)
		assert.Equal(t, "85.5", result)
	})

	t.Run("HighlightPassingString_SingleArg", func(t *testing.T) {
		result := HighlightPassingString("true")
		assert.Equal(t, "true", result)
	})

	t.Run("HighlightPassingString_MultipleArgs", func(t *testing.T) {
		result := HighlightPassingString("test-set-", 0)
		assert.Equal(t, "test-set-0", result)
	})

	t.Run("HighlightFailingString_SingleArg", func(t *testing.T) {
		result := HighlightFailingString("test-1")
		assert.Equal(t, "test-1", result)
	})

	t.Run("HighlightFailingString_MultipleArgs", func(t *testing.T) {
		result := HighlightFailingString("error: ", 404)
		assert.Equal(t, "error: 404", result)
	})

	t.Run("HighlightGrayString_SingleArg", func(t *testing.T) {
		result := HighlightGrayString("test-set-0")
		assert.Equal(t, "test-set-0", result)
	})

	t.Run("HighlightGrayString_NumericArg", func(t *testing.T) {
		result := HighlightGrayString(100)
		assert.Equal(t, "100", result)
	})
}

package report

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func TestApplyCliColorsToDiff_LargeLine(t *testing.T) {
	orig := models.IsAnsiDisabled
	t.Cleanup(func() {
		models.IsAnsiDisabled = orig
	})
	models.IsAnsiDisabled = false

	// Create a line larger than bufio.MaxScanTokenSize (64 KiB), e.g. 70 KiB
	largeExpected := strings.Repeat("A", 70*1024)
	actualValue := "small-actual-value"
	diff := fmt.Sprintf("Path: response.body\n  Expected: %s\n  Actual: %s\n", largeExpected, actualValue)

	result := applyCliColorsToDiff(diff)

	require.NotEmpty(t, result)
	assert.Contains(t, result, "Path: \x1b[33mresponse.body\x1b[0m")
	assert.Contains(t, result, "  Expected: \x1b[31m"+largeExpected+"\x1b[0m")
	assert.Contains(t, result, "  Actual: \x1b[32m"+actualValue+"\x1b[0m")
}

func TestApplyCliColorsToDiff(t *testing.T) {
	orig := models.IsAnsiDisabled
	t.Cleanup(func() {
		models.IsAnsiDisabled = orig
	})

	t.Run("empty string", func(t *testing.T) {
		models.IsAnsiDisabled = false
		assert.Equal(t, "", applyCliColorsToDiff(""))

		models.IsAnsiDisabled = true
		assert.Equal(t, "", applyCliColorsToDiff(""))
	})

	t.Run("ansi disabled preserves input", func(t *testing.T) {
		models.IsAnsiDisabled = true
		input := "Path: user.name\n  Expected: Alice\n  Actual: Bob\n"
		assert.Equal(t, input, applyCliColorsToDiff(input))
	})

	t.Run("normal multi-line diff", func(t *testing.T) {
		models.IsAnsiDisabled = false
		input := "Path: user.name\n  Expected: Alice\n  Actual: Bob"
		expected := "Path: \x1b[33muser.name\x1b[0m\n  Expected: \x1b[31mAlice\x1b[0m\n  Actual: \x1b[32mBob\x1b[0m"
		assert.Equal(t, expected, applyCliColorsToDiff(input))
	})

	t.Run("trailing newline omitted in output", func(t *testing.T) {
		models.IsAnsiDisabled = false
		inputWithNewline := "Path: status_code\n  Expected: 200\n  Actual: 500\n"
		inputWithoutNewline := "Path: status_code\n  Expected: 200\n  Actual: 500"
		assert.Equal(t, applyCliColorsToDiff(inputWithoutNewline), applyCliColorsToDiff(inputWithNewline))
	})

	t.Run("crlf endings", func(t *testing.T) {
		models.IsAnsiDisabled = false
		crlfInput := "Path: header.version\r\n  Expected: v1.0.0\r\n  Actual: v2.0.0\r\n"
		expected := "Path: \x1b[33mheader.version\x1b[0m\n  Expected: \x1b[31mv1.0.0\x1b[0m\n  Actual: \x1b[32mv2.0.0\x1b[0m"
		assert.Equal(t, expected, applyCliColorsToDiff(crlfInput))
	})

	t.Run("unmatched lines preserved via default branch", func(t *testing.T) {
		models.IsAnsiDisabled = false
		input := "=== CHANGES IN HEADERS ===\nPath: header.accept\n  Expected: text/plain\n  Actual: text/html\nNote: diff details"
		expected := "=== CHANGES IN HEADERS ===\nPath: \x1b[33mheader.accept\x1b[0m\n  Expected: \x1b[31mtext/plain\x1b[0m\n  Actual: \x1b[32mtext/html\x1b[0m\nNote: diff details"
		assert.Equal(t, expected, applyCliColorsToDiff(input))
	})
}

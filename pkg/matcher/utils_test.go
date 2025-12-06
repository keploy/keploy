package matcher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNormalizeArrayIndices(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple array index",
			input:    "items[0].id",
			expected: "items.0.id",
		},
		{
			name:     "nested array indices",
			input:    "items[0][1].name",
			expected: "items.0.1.name",
		},
		{
			name:     "no array index",
			input:    "items.id",
			expected: "items.id",
		},
		{
			name:     "multiple array indices",
			input:    "data[0].items[1].id",
			expected: "data.0.items.1.id",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only array index",
			input:    "[0]",
			expected: ".0",
		},
		{
			name:     "non-numeric bracket content",
			input:    "items[key].id",
			expected: "items[key].id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeArrayIndices(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRemoveNumericIndices(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "single numeric index",
			input:    "items.0.id",
			expected: "items.id",
		},
		{
			name:     "multiple numeric indices",
			input:    "items.0.1.name",
			expected: "items.name",
		},
		{
			name:     "no numeric indices",
			input:    "items.id",
			expected: "items.id",
		},
		{
			name:     "mixed indices",
			input:    "data.0.items.1.id",
			expected: "data.items.id",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only numeric",
			input:    "0",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := removeNumericIndices(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNoiseIndexMatch_ExactMatch(t *testing.T) {
	noise := map[string][]string{
		"items.0.id": {},
	}
	idx := buildNoiseIndex(noise)

	regs, isNoisy := idx.match("items.0.id")
	assert.True(t, isNoisy)
	_ = regs
}

func TestNoiseIndexMatch_AncestorMatch(t *testing.T) {
	noise := map[string][]string{
		"items.0": {},
	}
	idx := buildNoiseIndex(noise)

	regs, isNoisy := idx.match("items.0.id")
	assert.True(t, isNoisy)
	_ = regs
}

func TestNoiseIndexMatch_IndexlessFallback(t *testing.T) {
	noise := map[string][]string{
		"items.id": {},
	}
	idx := buildNoiseIndex(noise)

	regs, isNoisy := idx.match("items.0.id")
	assert.True(t, isNoisy)
	_ = regs
}

func TestNoiseIndexMatch_IndexlessFallbackAncestor(t *testing.T) {
	noise := map[string][]string{
		"items": {},
	}
	idx := buildNoiseIndex(noise)

	regs, isNoisy := idx.match("items.0.id")
	assert.True(t, isNoisy)
	_ = regs
}

func TestNoiseIndexMatch_NoMatch(t *testing.T) {
	noise := map[string][]string{
		"items.0.name": {},
	}
	idx := buildNoiseIndex(noise)

	regs, isNoisy := idx.match("items.0.id")
	assert.False(t, isNoisy)
	assert.Nil(t, regs)
}

func TestNoiseIndexMatch_WithRegexPatterns(t *testing.T) {
	noise := map[string][]string{
		"items.0.id": {`\d+`},
	}
	idx := buildNoiseIndex(noise)

	regs, isNoisy := idx.match("items.0.id")
	assert.True(t, isNoisy)
	assert.NotNil(t, regs)
	assert.Len(t, regs, 1)
}

func TestJSONDiffWithNoiseControl_NestedArrayWithIndex(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"items": [{"id": 1, "name": "test"}]}`
	actualJSON := `{"items": [{"id": 2, "name": "test"}]}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule with array index
	noise := map[string][]string{
		"items.0.id": {},
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because items.0.id is marked as noise")
}

func TestJSONDiffWithNoiseControl_NestedArrayIndexlessFallback(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"items": [{"id": 1, "name": "test"}]}`
	actualJSON := `{"items": [{"id": 2, "name": "test"}]}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule without array index (should match via indexless fallback)
	noise := map[string][]string{
		"items.id": {},
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because items.id (indexless) matches items.0.id")
}

func TestJSONDiffWithNoiseControl_NestedArrayWithBracketNotation(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"items": [{"id": 1, "name": "test"}]}`
	actualJSON := `{"items": [{"id": 2, "name": "test"}]}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule with bracket notation (should be normalized)
	noise := map[string][]string{
		"items[0].id": {},
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because items[0].id is normalized to items.0.id")
}

func TestJSONDiffWithNoiseControl_DeeplyNestedArray(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"data": {"items": [{"nested": [{"id": 1}]}]}}`
	actualJSON := `{"data": {"items": [{"nested": [{"id": 2}]}]}}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule for deeply nested array
	noise := map[string][]string{
		"data.items.0.nested.0.id": {},
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because deeply nested array path is marked as noise")
}

func TestJSONDiffWithNoiseControl_DeeplyNestedArrayIndexlessFallback(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"data": {"items": [{"nested": [{"id": 1}]}]}}`
	actualJSON := `{"data": {"items": [{"nested": [{"id": 2}]}]}}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule without indices (should match via indexless fallback)
	noise := map[string][]string{
		"data.items.nested.id": {},
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because indexless path matches nested array path")
}

func TestJSONDiffWithNoiseControl_MultipleArrayElements(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"items": [{"id": 1}, {"id": 2}]}`
	actualJSON := `{"items": [{"id": 3}, {"id": 4}]}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule for both array elements
	noise := map[string][]string{
		"items.id": {}, // Indexless fallback should match all elements
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because indexless items.id matches items.0.id and items.1.id")
}

func TestJSONDiffWithNoiseControl_AncestorMatch(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"items": [{"id": 1, "name": "test"}]}`
	actualJSON := `{"items": [{"id": 2, "name": "different"}]}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule for parent path (should match all children)
	noise := map[string][]string{
		"items.0": {}, // Ancestor match should cover items.0.id and items.0.name
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because items.0 is ancestor of items.0.id and items.0.name")
}

func TestJSONDiffWithNoiseControl_NoMatchWhenNotNoisy(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"items": [{"id": 1, "name": "test"}]}`
	actualJSON := `{"items": [{"id": 2, "name": "test"}]}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule for different field
	noise := map[string][]string{
		"items.0.name": {},
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.False(t, result.Matches(), "Should not match because items.0.id is not marked as noise")
}

func TestJSONDiffWithNoiseControl_RegexMatching(t *testing.T) {
	logger := zap.NewNop()

	expectedJSON := `{"items": [{"id": 1, "timestamp": "2024-01-01T00:00:00Z"}]}`
	actualJSON := `{"items": [{"id": 1, "timestamp": "2024-01-02T00:00:00Z"}]}`

	validatedJSON, err := ValidateAndMarshalJSON(logger, &expectedJSON, &actualJSON)
	require.NoError(t, err)

	// Noise rule with regex
	noise := map[string][]string{
		"items.0.timestamp": {`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`},
	}

	result, err := JSONDiffWithNoiseControl(validatedJSON, noise, false)
	require.NoError(t, err)
	assert.True(t, result.Matches(), "Should match because timestamp matches regex pattern")
}


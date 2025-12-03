package matcher

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestArrayMatchingWithIgnoreOrder_PrimitiveValues tests the fix for issue #2587
// where arrays like [1,1,1] would incorrectly match [1,2,3] with ignoreOrdering=true
func TestArrayMatchingWithIgnoreOrder_PrimitiveValues(t *testing.T) {
	tests := []struct {
		name          string
		expected      string
		actual        string
		ignoreOrder   bool
		shouldMatch   bool
		description   string
	}{
		{
			name:        "identical arrays with duplicates should match",
			expected:    `[1, 1, 1]`,
			actual:      `[1, 1, 1]`,
			ignoreOrder: true,
			shouldMatch: true,
			description: "Arrays with same duplicate values should match",
		},
		{
			name:        "different arrays should not match with ignore order",
			expected:    `[1, 1, 1]`,
			actual:      `[1, 2, 3]`,
			ignoreOrder: true,
			shouldMatch: false,
			description: "Bug #2587: [1,1,1] should NOT match [1,2,3] even with ignoreOrdering=true",
		},
		{
			name:        "different arrays should not match without ignore order",
			expected:    `[1, 1, 1]`,
			actual:      `[1, 2, 3]`,
			ignoreOrder: false,
			shouldMatch: false,
			description: "Arrays should not match when values are different",
		},
		{
			name:        "same elements different order should match with ignore order",
			expected:    `[1, 2, 3]`,
			actual:      `[3, 2, 1]`,
			ignoreOrder: true,
			shouldMatch: true,
			description: "Arrays with same elements in different order should match when ignoreOrdering=true",
		},
		{
			name:        "same elements different order should not match without ignore order",
			expected:    `[1, 2, 3]`,
			actual:      `[3, 2, 1]`,
			ignoreOrder: false,
			shouldMatch: false,
			description: "Arrays with same elements in different order should not match when ignoreOrdering=false",
		},
		{
			name:        "arrays with duplicates in different order should match",
			expected:    `[1, 2, 2, 3]`,
			actual:      `[2, 1, 3, 2]`,
			ignoreOrder: true,
			shouldMatch: true,
			description: "Arrays with same duplicate elements in different order should match",
		},
		{
			name:        "arrays with different duplicate counts should not match",
			expected:    `[1, 1, 2]`,
			actual:      `[1, 2, 2]`,
			ignoreOrder: true,
			shouldMatch: false,
			description: "Arrays with different counts of duplicates should not match",
		},
		{
			name:        "nested object in full JSON - the reported bug",
			expected:    `{"Code": 200, "Balance": 100, "List": [1, 1, 1]}`,
			actual:      `{"Code": 200, "Balance": 100, "List": [1, 2, 3]}`,
			ignoreOrder: true,
			shouldMatch: false,
			description: "Full bug report scenario: object with List field containing different values",
		},
		{
			name:        "arrays with null values should match",
			expected:    `[null, null, null]`,
			actual:      `[null, null, null]`,
			ignoreOrder: true,
			shouldMatch: true,
			description: "Arrays with null values should match",
		},
		{
			name:        "arrays with null in different order should match",
			expected:    `[null, 1, 2]`,
			actual:      `[1, null, 2]`,
			ignoreOrder: true,
			shouldMatch: true,
			description: "Arrays with null values in different order should match when ignoreOrdering=true",
		},
		{
			name:        "arrays with null vs numbers should not match",
			expected:    `[null, null]`,
			actual:      `[1, 2]`,
			ignoreOrder: true,
			shouldMatch: false,
			description: "Arrays with null values should not match arrays with numbers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse JSON strings
			var expectedJSON interface{}
			var actualJSON interface{}
			
			err := json.Unmarshal([]byte(tt.expected), &expectedJSON)
			assert.NoError(t, err, "Failed to unmarshal expected JSON")
			
			err = json.Unmarshal([]byte(tt.actual), &actualJSON)
			assert.NoError(t, err, "Failed to unmarshal actual JSON")

			// Call the matching function
			result, err := matchJSONWithNoiseHandlingIndexed(
				"",
				expectedJSON,
				actualJSON,
				noiseIndex{},
				map[string]bool{},
				tt.ignoreOrder,
			)

			// Assert
			assert.NoError(t, err, "matchJSONWithNoiseHandlingIndexed should not return error")
			
			if tt.shouldMatch {
				assert.True(t, result.matches, "Expected arrays to match: %s", tt.description)
			} else {
				assert.False(t, result.matches, "Expected arrays NOT to match: %s", tt.description)
			}
		})
	}
}

// TestArrayMatchingWithIgnoreOrder_StringValues tests string array matching
func TestArrayMatchingWithIgnoreOrder_StringValues(t *testing.T) {
	tests := []struct {
		name        string
		expected    string
		actual      string
		ignoreOrder bool
		shouldMatch bool
	}{
		{
			name:        "identical string arrays with duplicates",
			expected:    `["a", "a", "a"]`,
			actual:      `["a", "a", "a"]`,
			ignoreOrder: true,
			shouldMatch: true,
		},
		{
			name:        "different string arrays should not match",
			expected:    `["a", "a", "a"]`,
			actual:      `["a", "b", "c"]`,
			ignoreOrder: true,
			shouldMatch: false,
		},
		{
			name:        "same strings different order should match",
			expected:    `["a", "b", "c"]`,
			actual:      `["c", "b", "a"]`,
			ignoreOrder: true,
			shouldMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var expectedJSON interface{}
			var actualJSON interface{}
			
			err := json.Unmarshal([]byte(tt.expected), &expectedJSON)
			assert.NoError(t, err)
			
			err = json.Unmarshal([]byte(tt.actual), &actualJSON)
			assert.NoError(t, err)

			result, err := matchJSONWithNoiseHandlingIndexed(
				"",
				expectedJSON,
				actualJSON,
				noiseIndex{},
				map[string]bool{},
				tt.ignoreOrder,
			)

		assert.NoError(t, err)
		assert.Equal(t, tt.shouldMatch, result.matches)
	})
	}
}

// TestArrayMatchingWithIgnoreOrder_BooleanValues tests boolean array matching
func TestArrayMatchingWithIgnoreOrder_BooleanValues(t *testing.T) {
	tests := []struct {
		name        string
		expected    string
		actual      string
		ignoreOrder bool
		shouldMatch bool
	}{
		{
			name:        "boolean arrays in different order should match",
			expected:    `[true, true, false]`,
			actual:      `[true, false, true]`,
			ignoreOrder: true,
			shouldMatch: true,
		},
		{
			name:        "different boolean arrays should not match",
			expected:    `[true, true, true]`,
			actual:      `[true, false, false]`,
			ignoreOrder: true,
			shouldMatch: false,
		},
		{
			name:        "identical boolean arrays should match",
			expected:    `[true, false, true]`,
			actual:      `[true, false, true]`,
			ignoreOrder: false,
			shouldMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var expectedJSON interface{}
			var actualJSON interface{}
			
			err := json.Unmarshal([]byte(tt.expected), &expectedJSON)
			assert.NoError(t, err)
			
			err = json.Unmarshal([]byte(tt.actual), &actualJSON)
			assert.NoError(t, err)

			result, err := matchJSONWithNoiseHandlingIndexed(
				"",
				expectedJSON,
				actualJSON,
				noiseIndex{},
				map[string]bool{},
				tt.ignoreOrder,
			)

			assert.NoError(t, err)
			assert.Equal(t, tt.shouldMatch, result.matches)
		})
	}
}

// TestArrayMatchingWithIgnoreOrder_MixedPrimitiveTypes tests arrays with mixed primitive types
func TestArrayMatchingWithIgnoreOrder_MixedPrimitiveTypes(t *testing.T) {
	tests := []struct {
		name        string
		expected    string
		actual      string
		ignoreOrder bool
		shouldMatch bool
	}{
		{
			name:        "mixed primitives in different order should match",
			expected:    `[1, "a", true]`,
			actual:      `["a", true, 1]`,
			ignoreOrder: true,
			shouldMatch: true,
		},
		{
			name:        "mixed primitives with type mismatch should not match",
			expected:    `[1, "a", true]`,
			actual:      `[1, "b", true]`,
			ignoreOrder: true,
			shouldMatch: false,
		},
		{
			name:        "mixed primitives including null should match",
			expected:    `[1, null, "test"]`,
			actual:      `["test", 1, null]`,
			ignoreOrder: true,
			shouldMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var expectedJSON interface{}
			var actualJSON interface{}
			
			err := json.Unmarshal([]byte(tt.expected), &expectedJSON)
			assert.NoError(t, err)
			
			err = json.Unmarshal([]byte(tt.actual), &actualJSON)
			assert.NoError(t, err)

			result, err := matchJSONWithNoiseHandlingIndexed(
				"",
				expectedJSON,
				actualJSON,
				noiseIndex{},
				map[string]bool{},
				tt.ignoreOrder,
			)

			assert.NoError(t, err)
			assert.Equal(t, tt.shouldMatch, result.matches)
		})
	}
}
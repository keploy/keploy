package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateTableDiff(t *testing.T) {
	tests := []struct {
		name        string
		expected    string
		actual      string
		wantErr     bool
		wantContain []string
		wantExact   string
	}{
		{
			name:      "identical JSON objects",
			expected:  `{"name": "test", "value": 123}`,
			actual:    `{"name": "test", "value": 123}`,
			wantErr:   false,
			wantExact: "No differences found in JSON body after flattening.",
		},
		{
			name:     "simple value change",
			expected: `{"name": "test", "value": 123}`,
			actual:   `{"name": "test", "value": 456}`,
			wantErr:  false,
			wantContain: []string{
				"=== CHANGES WITHIN THE RESPONSE BODY ===",
				"Path: value",
				"Old: 123",
				"New: 456",
			},
		},
		{
			name:     "added field",
			expected: `{"name": "test"}`,
			actual:   `{"name": "test", "newField": "added"}`,
			wantErr:  false,
			wantContain: []string{
				"Path: newField",
				"Old: <added>",
				"New: \"added\"",
			},
		},
		{
			name:     "removed field",
			expected: `{"name": "test", "oldField": "removed"}`,
			actual:   `{"name": "test"}`,
			wantErr:  false,
			wantContain: []string{
				"Path: oldField",
				"Old: \"removed\"",
				"New: <removed>",
			},
		},
		{
			name:     "nested object changes",
			expected: `{"user": {"name": "John", "age": 30}}`,
			actual:   `{"user": {"name": "Jane", "age": 30}}`,
			wantErr:  false,
			wantContain: []string{
				"Path: user.name",
				"Old: \"John\"",
				"New: \"Jane\"",
			},
		},
		{
			name:     "array modifications",
			expected: `{"items": [1, 2, 3]}`,
			actual:   `{"items": [1, 2, 4]}`,
			wantErr:  false,
			wantContain: []string{
				"Path: items[2]",
				"Old: 3",
				"New: 4",
			},
		},
		{
			name:     "array length change",
			expected: `{"items": [1, 2]}`,
			actual:   `{"items": [1, 2, 3]}`,
			wantErr:  false,
			wantContain: []string{
				"Path: items[2]",
				"Old: <added>",
				"New: 3",
			},
		},
		{
			name:     "complex nested changes",
			expected: `{"users": [{"name": "John", "details": {"age": 30}}, {"name": "Jane"}]}`,
			actual:   `{"users": [{"name": "John", "details": {"age": 31}}, {"name": "Bob"}]}`,
			wantErr:  false,
			wantContain: []string{
				"Path: users[0].details.age",
				"Old: 30",
				"New: 31",
				"Path: users[1].name",
				"Old: \"Jane\"",
				"New: \"Bob\"",
			},
		},
		{
			name:     "invalid JSON in expected",
			expected: `{"invalid": json}`,
			actual:   `{"valid": "json"}`,
			wantErr:  false,
			wantContain: []string{
				"=== CHANGES WITHIN THE RESPONSE BODY ===",
				"Path: $",
			},
		},
		{
			name:     "invalid JSON in actual",
			expected: `{"valid": "json"}`,
			actual:   `{"invalid": json}`,
			wantErr:  false,
			wantContain: []string{
				"=== CHANGES WITHIN THE RESPONSE BODY ===",
				"Path: $",
			},
		},
		{
			name:      "empty JSON objects",
			expected:  `{}`,
			actual:    `{}`,
			wantErr:   false,
			wantExact: "No differences found in JSON body after flattening.",
		},
		{
			name:      "empty arrays",
			expected:  `[]`,
			actual:    `[]`,
			wantErr:   false,
			wantExact: "No differences found in JSON body after flattening.",
		},
		{
			name:      "null values",
			expected:  `{"value": null}`,
			actual:    `{"value": null}`,
			wantErr:   false,
			wantExact: "No differences found in JSON body after flattening.",
		},
		{
			name:     "number precision with different representations",
			expected: `{"price": 19.99}`,
			actual:   `{"price": 19.990}`,
			wantErr:  false,
			wantContain: []string{
				"=== CHANGES WITHIN THE RESPONSE BODY ===",
				"Path: price",
				"Old: 19.99",
				"New: 19.990",
			},
		},
		{
			name:     "string vs number",
			expected: `{"value": "123"}`,
			actual:   `{"value": 123}`,
			wantErr:  false,
			wantContain: []string{
				"Path: value",
				"Old: \"123\"",
				"New: 123",
			},
		},
		{
			name:     "boolean changes",
			expected: `{"enabled": true}`,
			actual:   `{"enabled": false}`,
			wantErr:  false,
			wantContain: []string{
				"Path: enabled",
				"Old: true",
				"New: false",
			},
		},
		{
			name:     "root array differences",
			expected: `[1, 2, 3]`,
			actual:   `[1, 2, 4]`,
			wantErr:  false,
			wantContain: []string{
				"Path: $[2]",
				"Old: 3",
				"New: 4",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GenerateTableDiff(tt.expected, tt.actual)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if tt.wantExact != "" {
				assert.Equal(t, tt.wantExact, result)
			} else {
				for _, want := range tt.wantContain {
					assert.Contains(t, result, want)
				}
			}
		})
	}
}

func TestParseJSONLoose(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
		wantErr  bool
	}{
		{
			name:     "valid object",
			input:    `{"name": "test"}`,
			wantType: "map[string]interface {}",
			wantErr:  false,
		},
		{
			name:     "valid array",
			input:    `[1, 2, 3]`,
			wantType: "[]interface {}",
			wantErr:  false,
		},
		{
			name:     "valid string",
			input:    `"hello"`,
			wantType: "string",
			wantErr:  false,
		},
		{
			name:     "valid number",
			input:    `123`,
			wantType: "json.Number",
			wantErr:  false,
		},
		{
			name:     "valid boolean",
			input:    `true`,
			wantType: "bool",
			wantErr:  false,
		},
		{
			name:     "valid null",
			input:    `null`,
			wantType: "<nil>",
			wantErr:  false,
		},
		{
			name:     "invalid JSON returns original string",
			input:    `{invalid json}`,
			wantType: "string",
			wantErr:  false,
		},
		{
			name:     "empty string",
			input:    ``,
			wantType: "string",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseJSONLoose(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantType, fmt.Sprintf("%T", result))
		})
	}
}

// Helper function for consistent type string formatting
func sprintf(format string, a ...interface{}) string {
	return fmt.Sprintf(format, a...)
}

func TestFlattenToMap(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected map[string]string
	}{
		{
			name:  "simple object",
			input: map[string]interface{}{"name": "test", "value": json.Number("123")},
			expected: map[string]string{
				"$.name":  `"test"`,
				"$.value": "123",
			},
		},
		{
			name:  "nested object",
			input: map[string]interface{}{"user": map[string]interface{}{"name": "John", "age": json.Number("30")}},
			expected: map[string]string{
				"$.user.name": `"John"`,
				"$.user.age":  "30",
			},
		},
		{
			name:  "array",
			input: []interface{}{json.Number("1"), json.Number("2"), json.Number("3")},
			expected: map[string]string{
				"$[0]": "1",
				"$[1]": "2",
				"$[2]": "3",
			},
		},
		{
			name:  "object with array",
			input: map[string]interface{}{"items": []interface{}{json.Number("1"), json.Number("2")}},
			expected: map[string]string{
				"$.items[0]": "1",
				"$.items[1]": "2",
			},
		},
		{
			name: "complex nested structure",
			input: map[string]interface{}{
				"users": []interface{}{
					map[string]interface{}{"name": "John", "age": json.Number("30")},
					map[string]interface{}{"name": "Jane", "age": json.Number("25")},
				},
			},
			expected: map[string]string{
				"$.users[0].name": `"John"`,
				"$.users[0].age":  "30",
				"$.users[1].name": `"Jane"`,
				"$.users[1].age":  "25",
			},
		},
		{
			name:  "primitive value",
			input: "simple string",
			expected: map[string]string{
				"$": `"simple string"`,
			},
		},
		{
			name:  "null value",
			input: nil,
			expected: map[string]string{
				"$": "null",
			},
		},
		{
			name:  "boolean value",
			input: true,
			expected: map[string]string{
				"$": "true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := make(map[string]string)
			flattenToMap(tt.input, "", result)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPathWithDollar(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty path",
			input:    "",
			expected: "$",
		},
		{
			name:     "path without dollar",
			input:    "user.name",
			expected: "$.user.name",
		},
		{
			name:     "path with dollar",
			input:    "$.user.name",
			expected: "$.user.name",
		},
		{
			name:     "root array path",
			input:    "[0]",
			expected: "$.[0]",
		},
		{
			name:     "complex path",
			input:    "users[0].details.name",
			expected: "$.users[0].details.name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pathWithDollar(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Fuzz tests
func FuzzGenerateTableDiff(f *testing.F) {
	// Seed with some valid JSON examples
	f.Add(`{"name": "test"}`, `{"name": "changed"}`)
	f.Add(`[1,2,3]`, `[1,2,4]`)
	f.Add(`{"nested": {"value": 123}}`, `{"nested": {"value": 456}}`)
	f.Add(`{}`, `{}`)
	f.Add(`[]`, `[]`)

	f.Fuzz(func(t *testing.T, expected, actual string) {
		// Function should not panic regardless of input
		_, err := GenerateTableDiff(expected, actual)
		// We only check that it doesn't panic, not specific error conditions
		// since fuzz testing with random strings will often produce invalid JSON
		_ = err
	})
}

func FuzzParseJSONLoose(f *testing.F) {
	// Seed with various JSON types
	f.Add(`{"key": "value"}`)
	f.Add(`[1, 2, 3]`)
	f.Add(`"string"`)
	f.Add(`123`)
	f.Add(`true`)
	f.Add(`null`)
	f.Add(`invalid json`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, input string) {
		// Function should not panic regardless of input
		result, err := parseJSONLoose(input)
		_ = result
		_ = err
	})
}

func FuzzFlattenToMap(f *testing.F) {
	// This is more complex since we need valid Go interface{} values
	// We'll test with JSON-parsed values
	validJSONs := []string{
		`{"name": "test", "value": 123}`,
		`[1, 2, 3]`,
		`{"nested": {"deep": {"value": "test"}}}`,
		`{"array": [{"item": 1}, {"item": 2}]}`,
		`null`,
		`true`,
		`"simple string"`,
		`123.456`,
	}

	for _, jsonStr := range validJSONs {
		f.Add(jsonStr)
	}

	f.Fuzz(func(t *testing.T, jsonStr string) {
		// Parse JSON and test flattening
		parsed, err := parseJSONLoose(jsonStr)
		if err != nil {
			return // Skip invalid JSON
		}

		result := make(map[string]string)
		// Function should not panic regardless of parsed structure
		flattenToMap(parsed, "", result)

		// Basic sanity check - result should have at least one entry
		assert.True(t, len(result) >= 1)
	})
}

func FuzzPathWithDollar(f *testing.F) {
	// Seed with various path patterns
	f.Add("")
	f.Add("simple")
	f.Add("$.already.prefixed")
	f.Add("nested.path.here")
	f.Add("[0]")
	f.Add("array[0].item")
	f.Add("$[0].item")

	f.Fuzz(func(t *testing.T, input string) {
		// Function should not panic and should always return a string starting with $
		result := pathWithDollar(input)
		assert.True(t, strings.HasPrefix(result, "$"))
	})
}

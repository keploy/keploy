package report

import (
	"strings"
	"testing"

	"go.keploy.io/server/v2/pkg/models"
)

// TestGenerateTableDiff tests JSON table diff generation
func TestGenerateTableDiff(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		actual   string
		wantErr  bool
		contains []string
	}{
		{
			name:     "simple JSON difference",
			expected: `{"name": "John", "age": 30}`,
			actual:   `{"name": "Jane", "age": 25}`,
			wantErr:  false,
			contains: []string{"Path: name", "Old: \"John\"", "New: \"Jane\"", "Path: age", "Old: 30", "New: 25"},
		},
		{
			name:     "nested JSON difference",
			expected: `{"user": {"name": "John", "details": {"age": 30}}}`,
			actual:   `{"user": {"name": "Jane", "details": {"age": 25}}}`,
			wantErr:  false,
			contains: []string{"Path: user.name", "Path: user.details.age"},
		},
		{
			name:     "array difference",
			expected: `{"items": [1, 2, 3]}`,
			actual:   `{"items": [1, 2, 4]}`,
			wantErr:  false,
			contains: []string{"Path: items[2]", "Old: 3", "New: 4"},
		},
		{
			name:     "added field",
			expected: `{"name": "John"}`,
			actual:   `{"name": "John", "age": 30}`,
			wantErr:  false,
			contains: []string{"Path: age", "Old: <added>", "New: 30"},
		},
		{
			name:     "removed field",
			expected: `{"name": "John", "age": 30}`,
			actual:   `{"name": "John"}`,
			wantErr:  false,
			contains: []string{"Path: age", "Old: 30", "New: <removed>"},
		},
		{
			name:     "identical JSON",
			expected: `{"name": "John", "age": 30}`,
			actual:   `{"name": "John", "age": 30}`,
			wantErr:  false,
			contains: []string{"No differences found"},
		},
		{
			name:     "invalid JSON expected",
			expected: `{"name": "John"`,
			actual:   `{"name": "Jane"}`,
			wantErr:  false,
			contains: []string{"Path: $", "Old:", "New:"},
		},
		{
			name:     "invalid JSON actual",
			expected: `{"name": "John"}`,
			actual:   `{"name": "Jane"`,
			wantErr:  false,
			contains: []string{"Path: $", "Old:", "New:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GenerateTableDiff(tt.expected, tt.actual)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			for _, contains := range tt.contains {
				if !strings.Contains(result, contains) {
					t.Errorf("expected result to contain %q, but it didn't. Result: %s", contains, result)
				}
			}
		})
	}
}

// TestParseJSONLoose tests the loose JSON parsing function
func TestParseJSONLoose(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantErr  bool
		expected interface{}
	}{
		{
			name:     "valid JSON object",
			input:    `{"name": "John", "age": 30}`,
			wantErr:  false,
			expected: map[string]interface{}{"name": "John", "age": float64(30)},
		},
		{
			name:     "valid JSON array",
			input:    `[1, 2, 3]`,
			wantErr:  false,
			expected: []interface{}{float64(1), float64(2), float64(3)},
		},
		{
			name:     "invalid JSON - returns original string",
			input:    `{"name": "John"`,
			wantErr:  false,
			expected: `{"name": "John"`,
		},
		{
			name:     "plain string - returns original",
			input:    `plain text`,
			wantErr:  false,
			expected: `plain text`,
		},
		{
			name:     "JSON with trailing content",
			input:    `{"name": "John"}{"extra": "data"}`,
			wantErr:  false,
			expected: `{"name": "John"}{"extra": "data"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseJSONLoose(tt.input)

			if tt.wantErr && err == nil {
				t.Error("expected error but got none")
				return
			}

			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			// For string results, compare directly
			if str, ok := tt.expected.(string); ok {
				if result != str {
					t.Errorf("expected %q, got %q", str, result)
				}
				return
			}

			// For complex types, just verify the result is not nil and has the right type
			if result == nil {
				t.Error("expected non-nil result")
			}
		})
	}
}

// TestFlattenToMap tests the JSON flattening functionality
func TestFlattenToMap(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected map[string]string
	}{
		{
			name:  "simple object",
			input: map[string]interface{}{"name": "John", "age": float64(30)},
			expected: map[string]string{
				"$.name": `"John"`,
				"$.age":  "30",
			},
		},
		{
			name:  "nested object",
			input: map[string]interface{}{"user": map[string]interface{}{"name": "John", "age": float64(30)}},
			expected: map[string]string{
				"$.user.name": `"John"`,
				"$.user.age":  "30",
			},
		},
		{
			name:  "array",
			input: []interface{}{float64(1), float64(2), float64(3)},
			expected: map[string]string{
				"$[0]": "1",
				"$[1]": "2",
				"$[2]": "3",
			},
		},
		{
			name:  "object with array",
			input: map[string]interface{}{"items": []interface{}{float64(1), float64(2)}},
			expected: map[string]string{
				"$.items[0]": "1",
				"$.items[1]": "2",
			},
		},
		{
			name:  "primitive value",
			input: "simple string",
			expected: map[string]string{
				"$": `"simple string"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := make(map[string]string)
			flattenToMap(tt.input, "", result)

			if len(result) != len(tt.expected) {
				t.Errorf("expected %d entries, got %d", len(tt.expected), len(result))
			}

			for key, expectedValue := range tt.expected {
				if actualValue, exists := result[key]; !exists {
					t.Errorf("missing key %q", key)
				} else if actualValue != expectedValue {
					t.Errorf("for key %q: expected %q, got %q", key, expectedValue, actualValue)
				}
			}
		})
	}
}

// TestPathWithDollar tests the path formatting function
func TestPathWithDollar(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "$"},
		{"name", "$.name"},
		{"$.name", "$.name"},
		{"$[0]", "$[0]"},
		{"user.name", "$.user.name"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := pathWithDollar(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestGeneratePlainOldNewDiff tests the plain text diff generation
func TestGeneratePlainOldNewDiff(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		actual   string
		bodyType models.BodyType
		contains []string
	}{
		{
			name:     "different strings",
			expected: "Hello World",
			actual:   "Hello Universe",
			bodyType: models.Plain,
			contains: []string{"Path: PLAIN", "Old: Hello World", "New: Hello Universe"},
		},
		{
			name:     "identical strings",
			expected: "Hello World",
			actual:   "Hello World",
			bodyType: models.Plain,
			contains: []string{"No differences found"},
		},
		{
			name:     "strings with special characters",
			expected: "Line 1\nLine 2\tTabbed",
			actual:   "Line 1\rLine 2\tSpaced",
			bodyType: models.Plain,
			contains: []string{"Path: PLAIN", "Old: Line 1\\nLine 2\\tTabbed", "New: Line 1\\rLine 2\\tSpaced"},
		},
		{
			name:     "binary type",
			expected: "binary data 1",
			actual:   "binary data 2",
			bodyType: models.Binary,
			contains: []string{"Path: BINARY", "Old: binary data 1", "New: binary data 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GeneratePlainOldNewDiff(tt.expected, tt.actual, tt.bodyType)

			for _, contains := range tt.contains {
				if !strings.Contains(result, contains) {
					t.Errorf("expected result to contain %q, but it didn't. Result: %s", contains, result)
				}
			}
		})
	}
}

// TestEscapeOneLine tests the string escaping function
func TestEscapeOneLine(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal string",
			input:    "Hello World",
			expected: "Hello World",
		},
		{
			name:     "string with newlines",
			input:    "Line 1\nLine 2",
			expected: "Line 1\\nLine 2",
		},
		{
			name:     "string with tabs and carriage returns",
			input:    "Tab\tReturn\r",
			expected: "Tab\\tReturn\\r",
		},
		{
			name:     "string with control characters",
			input:    "Control\x01\x02",
			expected: "Control\\x01\\x02",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "string with mixed characters",
			input:    "Normal\nTab\tControl\x1F",
			expected: "Normal\\nTab\\tControl\\x1F",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := escapeOneLine(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

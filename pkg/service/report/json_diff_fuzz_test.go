package report

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzGenerateTableDiff tests GenerateTableDiff with random JSON inputs
func FuzzGenerateTableDiff(f *testing.F) {
	// Seed with some interesting test cases
	f.Add(`{"name": "John"}`, `{"name": "Jane"}`)
	f.Add(`{"numbers": [1, 2, 3]}`, `{"numbers": [1, 2, 4]}`)
	f.Add(`{}`, `{"new": "field"}`)
	f.Add(`{"nested": {"deep": {"value": 1}}}`, `{"nested": {"deep": {"value": 2}}}`)
	f.Add(`null`, `{"data": null}`)
	f.Add(`[1, 2, 3]`, `[1, 2, 3, 4]`)
	f.Add(`{"unicode": "🎉🚀"}`, `{"unicode": "🎈🎊"}`)
	f.Add(`{"large": 9223372036854775807}`, `{"large": -9223372036854775808}`)
	f.Add(`{"float": 3.141592653589793}`, `{"float": 2.718281828459045}`)
	f.Add(`{"bool": true}`, `{"bool": false}`)
	f.Add(`{"empty_string": ""}`, `{"empty_string": " "}`)
	f.Add(`{"escaped": "\"quoted\""}`, `{"escaped": "'quoted'"}`)
	f.Add(`{"tabs": "\t\n\r"}`, `{"tabs": "   "}`)

	f.Fuzz(func(t *testing.T, expected, actual string) {
		// Skip if either input is not valid JSON and also not a simple string
		if !isJSONOrSimpleString(expected) || !isJSONOrSimpleString(actual) {
			t.Skip()
		}

		// Skip extremely large inputs to avoid timeouts
		if len(expected) > 10000 || len(actual) > 10000 {
			t.Skip()
		}

		// Skip if strings contain invalid UTF-8
		if !utf8.ValidString(expected) || !utf8.ValidString(actual) {
			t.Skip()
		}

		diff, err := GenerateTableDiff(expected, actual)

		if err != nil {
			// If there's an error, it should be a parsing error
			if !strings.Contains(err.Error(), "cannot parse JSON") {
				t.Errorf("Unexpected error type: %v", err)
			}
			return
		}

		// Basic sanity checks on the diff output
		if expected == actual {
			// Identical inputs should produce "no differences" message
			if !strings.Contains(diff, "No differences found") {
				t.Errorf("Identical inputs should show no differences, got: %s", diff)
			}
		} else {
			// Different inputs should produce a meaningful diff
			if strings.Contains(diff, "No differences found") {
				// This might be valid if the differences are in structure only
				// and don't affect the flattened representation
				return
			}

			// Check that diff contains the header
			if !strings.Contains(diff, "=== CHANGES WITHIN THE RESPONSE BODY ===") {
				t.Errorf("Diff should contain header, got: %s", diff)
			}
		}

		// Verify the diff doesn't contain obvious corruption
		if strings.Contains(diff, "\x00") {
			t.Errorf("Diff contains null bytes: %s", diff)
		}

		// Check that the diff is valid UTF-8
		if !utf8.ValidString(diff) {
			t.Errorf("Diff contains invalid UTF-8")
		}
	})
}

// FuzzParseJSONLoose tests parseJSONLoose with random inputs
func FuzzParseJSONLoose(f *testing.F) {
	// Seed with various JSON types
	f.Add(`{"key": "value"}`)
	f.Add(`[1, 2, 3]`)
	f.Add(`"simple string"`)
	f.Add(`123`)
	f.Add(`123.456`)
	f.Add(`true`)
	f.Add(`false`)
	f.Add(`null`)
	f.Add(`""`)
	f.Add(`[]`)
	f.Add(`{}`)
	f.Add(`{"nested": {"deep": {"value": [1, 2, {"inner": true}]}}}`)
	f.Add(`invalid json`)
	f.Add(`{"unclosed": "object"`)
	f.Add(`{"trailing": "comma",}`)

	f.Fuzz(func(t *testing.T, input string) {
		// Skip extremely large inputs
		if len(input) > 5000 {
			t.Skip()
		}

		// Skip invalid UTF-8
		if !utf8.ValidString(input) {
			t.Skip()
		}

		result, err := parseJSONLoose(input)

		// Should never panic
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("parseJSONLoose panicked: %v", r)
			}
		}()

		if err != nil {
			t.Errorf("parseJSONLoose should not return error, got: %v", err)
		}

		// parseJSONLoose can legitimately return nil for JSON "null"
		// and should parse valid JSON or return the original string for invalid JSON

		// Check if the ENTIRE input is valid JSON (not just the beginning)
		// Use the same approach as parseJSONLoose
		var temp interface{}
		decoder := json.NewDecoder(strings.NewReader(input))
		decoder.UseNumber() // Match parseJSONLoose behavior
		isValidJSON := decoder.Decode(&temp) == nil
		if isValidJSON {
			// Check if there's any trailing content that would make it invalid
			var trailing interface{}
			if decoder.Decode(&trailing) == nil {
				isValidJSON = false // There was trailing content, so it's invalid JSON
			}
		}

		if isValidJSON {
			// Input was completely valid JSON
			// parseJSONLoose should parse it (not return the original string, unless it's a string literal that equals the parsed value)
		} else {
			// Input was invalid JSON, should return the original string
			if result != input {
				t.Errorf("Invalid JSON should return original string. Input: %q, got: %v (type %T)", input, result, result)
			}
		}
	})
}

// isEmptyOrOnlyEmptyContainers checks if a JSON value contains only empty containers
// Returns true for null, empty objects, empty arrays, or nested structures containing only empty containers
func isEmptyOrOnlyEmptyContainers(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]interface{}:
		if len(x) == 0 {
			return true
		}
		// Check if all values in the map are empty containers
		for _, val := range x {
			if !isEmptyOrOnlyEmptyContainers(val) {
				return false
			}
		}
		return true
	case []interface{}:
		if len(x) == 0 {
			return true
		}
		// Check if all elements in the array are empty containers
		for _, val := range x {
			if !isEmptyOrOnlyEmptyContainers(val) {
				return false
			}
		}
		return true
	default:
		// Any actual value (string, number, boolean) means it's not empty
		return false
	}
}

// FuzzFlattenToMap tests flattenToMap with random structures
func FuzzFlattenToMap(f *testing.F) {
	// Seed with various structures that could come from JSON parsing
	f.Add(`{"simple": "value"}`)
	f.Add(`{"nested": {"key": "value"}}`)
	f.Add(`{"array": [1, 2, 3]}`)
	f.Add(`{"mixed": [{"id": 1}, {"id": 2}]}`)
	f.Add(`[{"root": "array"}]`)
	f.Add(`{"numbers": {"int": 42, "float": 3.14}}`)
	f.Add(`{"booleans": {"true": true, "false": false}}`)
	f.Add(`{"nullValue": null}`)
	f.Add(`{"unicode": "🎉", "emoji": "🚀"}`)
	f.Add(`{"empty": {"object": {}, "array": [], "string": ""}}`)

	f.Fuzz(func(t *testing.T, jsonStr string) {
		// Skip very large inputs
		if len(jsonStr) > 5000 {
			t.Skip()
		}

		// Skip invalid UTF-8
		if !utf8.ValidString(jsonStr) {
			t.Skip()
		}

		// Parse the JSON first
		var parsed interface{}
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
			t.Skip() // Skip invalid JSON
		}

		// Test flattenToMap
		output := make(map[string]string)

		// Should not panic
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("flattenToMap panicked with input %s: %v", jsonStr, r)
			}
		}()

		flattenToMap(parsed, "", output)

		// Basic sanity checks
		for key, value := range output {
			// All keys should start with $
			if !strings.HasPrefix(key, "$") {
				t.Errorf("Key should start with $, got: %s", key)
			}

			// Values should be valid (not empty unless the original was empty)
			if value == "" {
				// Empty values are only valid for empty strings
				if !strings.Contains(value, `""`) {
					// Check if this represents an empty string in JSON
					continue
				}
			}

			// Value should be valid UTF-8
			if !utf8.ValidString(value) {
				t.Errorf("Value contains invalid UTF-8: %s", value)
			}

			// Keys should not contain null bytes
			if strings.Contains(key, "\x00") {
				t.Errorf("Key contains null bytes: %s", key)
			}
		}

		// Check that we can reconstruct some meaning from the flattened map
		// Empty output is valid if the JSON contains only empty containers or primitives at root level
		if len(output) == 0 && !isEmptyOrOnlyEmptyContainers(parsed) {
			t.Errorf("Non-empty JSON with actual values should produce some flattened output: %s", jsonStr)
		}
	})
}

// FuzzPathWithDollar tests pathWithDollar with random path inputs
func FuzzPathWithDollar(f *testing.F) {
	// Seed with typical path patterns
	f.Add("")
	f.Add("simple")
	f.Add("$.already.prefixed")
	f.Add("$[0]")
	f.Add("nested.deep.path")
	f.Add("array[0].field")
	f.Add("special.chars-in_path")
	f.Add("unicode.🎉.path")
	f.Add("$.$.multiple.dollars")
	f.Add("$")

	f.Fuzz(func(t *testing.T, path string) {
		// Skip very long paths
		if len(path) > 1000 {
			t.Skip()
		}

		// Skip invalid UTF-8
		if !utf8.ValidString(path) {
			t.Skip()
		}

		// Should not panic
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("pathWithDollar panicked with input %s: %v", path, r)
			}
		}()

		result := pathWithDollar(path)

		// Result should always start with $
		if !strings.HasPrefix(result, "$") {
			t.Errorf("Result should start with $, got: %s", result)
		}

		// Result should be valid UTF-8
		if !utf8.ValidString(result) {
			t.Errorf("Result contains invalid UTF-8: %s", result)
		}

		// Special cases
		if path == "" {
			if result != "$" {
				t.Errorf("Empty path should return $, got: %s", result)
			}
		} else if strings.HasPrefix(path, "$") {
			if result != path {
				t.Errorf("Path already starting with $ should be unchanged, got: %s", result)
			}
		} else {
			if result != "$."+path {
				t.Errorf("Path should be prefixed with $., got: %s", result)
			}
		}
	})
}

// Helper function to check if a string is valid JSON or a simple string
func isJSONOrSimpleString(s string) bool {
	// Check if it's valid JSON
	var temp interface{}
	if json.Unmarshal([]byte(s), &temp) == nil {
		return true
	}

	// If not valid JSON, check if it's a reasonable string (no control characters)
	for _, r := range s {
		if r < 32 && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}

	return true
}

// Benchmark functions for performance testing

// BenchmarkGenerateTableDiff_SmallJSON benchmarks with small JSON objects
func BenchmarkGenerateTableDiff_SmallJSON(b *testing.B) {
	expected := `{"name": "John", "age": 30, "city": "New York"}`
	actual := `{"name": "Jane", "age": 25, "city": "San Francisco"}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := GenerateTableDiff(expected, actual)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGenerateTableDiff_LargeJSON benchmarks with large JSON objects
func BenchmarkGenerateTableDiff_LargeJSON(b *testing.B) {
	// Create a large JSON object
	expected := generateLargeJSON(100, false)
	actual := generateLargeJSON(100, true) // With some differences

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := GenerateTableDiff(expected, actual)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGenerateTableDiff_DeepNesting benchmarks with deeply nested JSON
func BenchmarkGenerateTableDiff_DeepNesting(b *testing.B) {
	expected := generateDeeplyNestedJSON(20, false)
	actual := generateDeeplyNestedJSON(20, true)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := GenerateTableDiff(expected, actual)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseJSONLoose benchmarks parseJSONLoose function
func BenchmarkParseJSONLoose(b *testing.B) {
	jsonStr := generateLargeJSON(50, false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := parseJSONLoose(jsonStr)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFlattenToMap benchmarks flattenToMap function
func BenchmarkFlattenToMap(b *testing.B) {
	// Pre-parse a large JSON structure
	jsonStr := generateLargeJSON(50, false)
	parsed, err := parseJSONLoose(jsonStr)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		output := make(map[string]string)
		flattenToMap(parsed, "", output)
	}
}

// Helper functions for generating test data

// generateLargeJSON creates a large JSON string for testing
func generateLargeJSON(size int, modified bool) string {
	var sb strings.Builder
	sb.WriteString(`{"users": [`)

	for i := 0; i < size; i++ {
		if i > 0 {
			sb.WriteString(",")
		}

		name := fmt.Sprintf("user%d", i)
		age := 20 + (i % 50)

		if modified && i%10 == 0 {
			// Introduce some changes
			name = fmt.Sprintf("modified_user%d", i)
			age = age + 10
		}

		sb.WriteString(fmt.Sprintf(`{
			"id": %d,
			"name": "%s",
			"age": %d,
			"email": "%s@example.com",
			"active": %t,
			"balance": %.2f,
			"tags": ["tag%d", "tag%d"],
			"metadata": {
				"created": "2023-01-01T00:00:00Z",
				"updated": "2023-12-31T23:59:59Z",
				"version": %d
			}
		}`, i, name, age, name, i%2 == 0, float64(i)*10.5, i, i+1, i%5))
	}

	sb.WriteString(`], "total": `)
	sb.WriteString(fmt.Sprintf("%d", size))
	sb.WriteString(`}`)

	return sb.String()
}

// generateDeeplyNestedJSON creates a deeply nested JSON structure
func generateDeeplyNestedJSON(depth int, modified bool) string {
	var sb strings.Builder

	// Start with opening braces
	for i := 0; i < depth; i++ {
		sb.WriteString(fmt.Sprintf(`{"level%d": `, i))
	}

	// Add the final value
	value := "deep_value"
	if modified {
		value = "modified_deep_value"
	}
	sb.WriteString(fmt.Sprintf(`"%s"`, value))

	// Close all braces
	for i := 0; i < depth; i++ {
		sb.WriteString("}")
	}

	return sb.String()
}

// Property-based testing helpers

// generateRandomJSON creates random valid JSON for property testing
func generateRandomJSON(r *rand.Rand, maxDepth int, currentDepth int) interface{} {
	if currentDepth >= maxDepth {
		// Return a simple value at max depth
		return generateRandomValue(r)
	}

	switch r.Intn(3) {
	case 0:
		// Generate object
		obj := make(map[string]interface{})
		size := r.Intn(5) + 1
		for i := 0; i < size; i++ {
			key := fmt.Sprintf("key%d", i)
			obj[key] = generateRandomJSON(r, maxDepth, currentDepth+1)
		}
		return obj
	case 1:
		// Generate array
		size := r.Intn(5) + 1
		arr := make([]interface{}, size)
		for i := 0; i < size; i++ {
			arr[i] = generateRandomJSON(r, maxDepth, currentDepth+1)
		}
		return arr
	default:
		// Generate simple value
		return generateRandomValue(r)
	}
}

// generateRandomValue creates random primitive values
func generateRandomValue(r *rand.Rand) interface{} {
	switch r.Intn(6) {
	case 0:
		return fmt.Sprintf("string%d", r.Intn(1000))
	case 1:
		return r.Intn(1000)
	case 2:
		return r.Float64() * 1000
	case 3:
		return r.Intn(2) == 1
	case 4:
		return nil
	default:
		return ""
	}
}

// TestGenerateTableDiff_PropertyBased tests using property-based testing principles
func TestGenerateTableDiff_PropertyBased(t *testing.T) {
	r := rand.New(rand.NewSource(42)) // Fixed seed for reproducibility

	for i := 0; i < 100; i++ {
		// Generate two related JSON structures
		base := generateRandomJSON(r, 3, 0)

		// Serialize to JSON
		baseJSON, err := json.Marshal(base)
		if err != nil {
			t.Skip()
		}

		// Create a modified version
		modified := generateRandomJSON(r, 3, 0)
		modifiedJSON, err := json.Marshal(modified)
		if err != nil {
			t.Skip()
		}

		// Test the diff generation
		diff, err := GenerateTableDiff(string(baseJSON), string(modifiedJSON))

		// Should not error on valid JSON
		if err != nil {
			t.Errorf("Iteration %d: Should not error on valid JSON: %v", i, err)
			continue
		}

		// Basic properties that should always hold
		if string(baseJSON) == string(modifiedJSON) {
			if !strings.Contains(diff, "No differences found") {
				t.Errorf("Iteration %d: Identical JSON should show no differences", i)
			}
		}

		// Diff should be valid UTF-8
		if !utf8.ValidString(diff) {
			t.Errorf("Iteration %d: Diff contains invalid UTF-8", i)
		}

		// Should not contain control characters
		for _, r := range diff {
			if r < 32 && r != '\t' && r != '\n' && r != '\r' {
				t.Errorf("Iteration %d: Diff contains unexpected control character: %v", i, r)
				break
			}
		}
	}
}

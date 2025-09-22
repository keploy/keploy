package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateTableDiff_IdenticalJSON_001 tests that identical JSON strings return no differences
func TestGenerateTableDiff_IdenticalJSON_001(t *testing.T) {
	expected := `{"name": "John", "age": 30, "city": "New York"}`
	actual := `{"name": "John", "age": 30, "city": "New York"}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Equal(t, "No differences found in JSON body after flattening.", diff)
}

// TestGenerateTableDiff_SimpleFieldChanges_002 tests basic field value changes
func TestGenerateTableDiff_SimpleFieldChanges_002(t *testing.T) {
	expected := `{"name": "John", "age": 30, "city": "New York"}`
	actual := `{"name": "Jane", "age": 25, "city": "New York"}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "=== CHANGES WITHIN THE RESPONSE BODY ===")
	assert.Contains(t, diff, "Path: age")
	assert.Contains(t, diff, "Expected: 30")
	assert.Contains(t, diff, "Actual: 25")
	assert.Contains(t, diff, "Path: name")
	assert.Contains(t, diff, `Expected: "John"`)
	assert.Contains(t, diff, `Actual: "Jane"`)
}

// TestGenerateTableDiff_FieldAddition_003 tests when fields are added to the JSON
func TestGenerateTableDiff_FieldAddition_003(t *testing.T) {
	expected := `{"name": "John", "age": 30}`
	actual := `{"name": "John", "age": 30, "city": "New York", "country": "USA"}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: city")
	assert.Contains(t, diff, "Expected: <added>")
	assert.Contains(t, diff, `Actual: "New York"`)
	assert.Contains(t, diff, "Path: country")
	assert.Contains(t, diff, "Expected: <added>")
	assert.Contains(t, diff, `Actual: "USA"`)
}

// TestGenerateTableDiff_FieldRemoval_004 tests when fields are removed from the JSON
func TestGenerateTableDiff_FieldRemoval_004(t *testing.T) {
	expected := `{"name": "John", "age": 30, "city": "New York", "country": "USA"}`
	actual := `{"name": "John", "age": 30}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: city")
	assert.Contains(t, diff, `Expected: "New York"`)
	assert.Contains(t, diff, "Actual: <removed>")
	assert.Contains(t, diff, "Path: country")
	assert.Contains(t, diff, `Expected: "USA"`)
	assert.Contains(t, diff, "Actual: <removed>")
}

// TestGenerateTableDiff_NestedObjects_005 tests nested object differences
func TestGenerateTableDiff_NestedObjects_005(t *testing.T) {
	expected := `{"user": {"name": "John", "details": {"age": 30, "city": "NY"}}}`
	actual := `{"user": {"name": "Jane", "details": {"age": 25, "city": "NY"}}}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: user.details.age")
	assert.Contains(t, diff, "Expected: 30")
	assert.Contains(t, diff, "Actual: 25")
	assert.Contains(t, diff, "Path: user.name")
	assert.Contains(t, diff, `Expected: "John"`)
	assert.Contains(t, diff, `Actual: "Jane"`)
}

// TestGenerateTableDiff_Arrays_006 tests array handling and differences
func TestGenerateTableDiff_Arrays_006(t *testing.T) {
	expected := `{"items": [{"id": 1, "name": "item1"}, {"id": 2, "name": "item2"}]}`
	actual := `{"items": [{"id": 1, "name": "updated_item1"}, {"id": 3, "name": "item3"}]}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: items[0].name")
	assert.Contains(t, diff, `Expected: "item1"`)
	assert.Contains(t, diff, `Actual: "updated_item1"`)
	assert.Contains(t, diff, "Path: items[1].id")
	assert.Contains(t, diff, "Expected: 2")
	assert.Contains(t, diff, "Actual: 3")
	assert.Contains(t, diff, "Path: items[1].name")
	assert.Contains(t, diff, `Expected: "item2"`)
	assert.Contains(t, diff, `Actual: "item3"`)
}

// TestGenerateTableDiff_RootArray_007 tests when the root JSON is an array
func TestGenerateTableDiff_RootArray_007(t *testing.T) {
	expected := `[{"id": 1, "name": "first"}, {"id": 2, "name": "second"}]`
	actual := `[{"id": 1, "name": "updated_first"}, {"id": 2, "name": "second"}]`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: $[0].name")
	assert.Contains(t, diff, `Expected: "first"`)
	assert.Contains(t, diff, `Actual: "updated_first"`)
}

// TestGenerateTableDiff_DifferentTypes_008 tests when field types change
func TestGenerateTableDiff_DifferentTypes_008(t *testing.T) {
	expected := `{"value": 123, "flag": true, "data": null}`
	actual := `{"value": "123", "flag": "true", "data": {}}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: data")
	assert.Contains(t, diff, "Expected: null")
	assert.Contains(t, diff, "Actual: <removed>")
	assert.Contains(t, diff, "Path: flag")
	assert.Contains(t, diff, "Expected: true")
	assert.Contains(t, diff, `Actual: "true"`)
	assert.Contains(t, diff, "Path: value")
	assert.Contains(t, diff, "Expected: 123")
	assert.Contains(t, diff, `Actual: "123"`)
}

// TestGenerateTableDiff_InvalidJSON_009 tests behavior with invalid JSON input
func TestGenerateTableDiff_InvalidJSON_009(t *testing.T) {
	expected := `{"valid": "json"}`
	actual := `{"invalid": json}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.NotEmpty(t, diff)
	assert.Contains(t, diff, "=== CHANGES WITHIN THE RESPONSE BODY ===")
	assert.NotContains(t, diff, "No differences found")
}

// TestGenerateTableDiff_EmptyJSON_010 tests empty JSON objects
func TestGenerateTableDiff_EmptyJSON_010(t *testing.T) {
	expected := `{}`
	actual := `{}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Equal(t, "No differences found in JSON body after flattening.", diff)
}

// TestGenerateTableDiff_EmptyToPopulated_011 tests from empty to populated JSON
func TestGenerateTableDiff_EmptyToPopulated_011(t *testing.T) {
	expected := `{}`
	actual := `{"name": "John", "age": 30}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: age")
	assert.Contains(t, diff, "Expected: <added>")
	assert.Contains(t, diff, "Actual: 30")
	assert.Contains(t, diff, "Path: name")
	assert.Contains(t, diff, "Expected: <added>")
	assert.Contains(t, diff, `Actual: "John"`)
}

// TestGenerateTableDiff_LargeNumbers_012 tests handling of large numbers
func TestGenerateTableDiff_LargeNumbers_012(t *testing.T) {
	expected := `{"bigNum": 9223372036854775807, "decimal": 123.456789}`
	actual := `{"bigNum": 9223372036854775806, "decimal": 123.456788}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: bigNum")
	assert.Contains(t, diff, "Expected: 9223372036854775807")
	assert.Contains(t, diff, "Actual: 9223372036854775806")
	assert.Contains(t, diff, "Path: decimal")
	assert.Contains(t, diff, "Expected: 123.456789")
	assert.Contains(t, diff, "Actual: 123.456788")
}

// TestGenerateTableDiff_SpecialCharacters_013 tests strings with special characters
func TestGenerateTableDiff_SpecialCharacters_013(t *testing.T) {
	expected := `{"message": "Hello\nWorld", "emoji": "🎉", "quote": "He said \"Hello\""}`
	actual := `{"message": "Hello\tWorld", "emoji": "🚀", "quote": "She said \"Hi\""}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: emoji")
	assert.Contains(t, diff, `Expected: "🎉"`)
	assert.Contains(t, diff, `Actual: "🚀"`)
	assert.Contains(t, diff, "Path: message")
	assert.Contains(t, diff, `"Hello\nWorld"`)
	assert.Contains(t, diff, `"Hello\tWorld"`)
	assert.Contains(t, diff, "Path: quote")
	assert.Contains(t, diff, `"He said \"Hello\""`)
	assert.Contains(t, diff, `"She said \"Hi\""`)
}

// TestGenerateTableDiff_ComplexNesting_014 tests deeply nested structures
func TestGenerateTableDiff_ComplexNesting_014(t *testing.T) {
	expected := `{
		"level1": {
			"level2": {
				"level3": {
					"level4": {
						"value": "deep"
					}
				}
			}
		}
	}`
	actual := `{
		"level1": {
			"level2": {
				"level3": {
					"level4": {
						"value": "deeper"
					}
				}
			}
		}
	}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: level1.level2.level3.level4.value")
	assert.Contains(t, diff, `Expected: "deep"`)
	assert.Contains(t, diff, `Actual: "deeper"`)
}

// TestGenerateTableDiff_ArraySizeChange_015 tests when array sizes differ
func TestGenerateTableDiff_ArraySizeChange_015(t *testing.T) {
	expected := `{"items": [1, 2, 3]}`
	actual := `{"items": [1, 2, 3, 4, 5]}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: items[3]")
	assert.Contains(t, diff, "Expected: <added>")
	assert.Contains(t, diff, "Actual: 4")
	assert.Contains(t, diff, "Path: items[4]")
	assert.Contains(t, diff, "Expected: <added>")
	assert.Contains(t, diff, "Actual: 5")
}

// TestParseJSONLoose_ValidJSON_016 tests parseJSONLoose with valid JSON
func TestParseJSONLoose_ValidJSON_016(t *testing.T) {
	jsonStr := `{"name": "John", "age": 30, "active": true}`

	result, err := parseJSONLoose(jsonStr)

	require.NoError(t, err)
	resultMap, ok := result.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "John", resultMap["name"])
	assert.Equal(t, json.Number("30"), resultMap["age"])
	assert.Equal(t, true, resultMap["active"])
}

// TestParseJSONLoose_InvalidJSON_017 tests parseJSONLoose with invalid JSON
func TestParseJSONLoose_InvalidJSON_017(t *testing.T) {
	jsonStr := `invalid json string`

	result, err := parseJSONLoose(jsonStr)

	require.NoError(t, err)
	assert.Equal(t, jsonStr, result) // Should return the original string
}

// TestParseJSONLoose_NumberPreservation_018 tests that numbers are preserved as json.Number
func TestParseJSONLoose_NumberPreservation_018(t *testing.T) {
	jsonStr := `{"bigInt": 9223372036854775807, "float": 123.456789}`

	result, err := parseJSONLoose(jsonStr)

	require.NoError(t, err)
	resultMap, ok := result.(map[string]interface{})
	require.True(t, ok)
	assert.IsType(t, json.Number(""), resultMap["bigInt"])
	assert.IsType(t, json.Number(""), resultMap["float"])
	assert.Equal(t, "9223372036854775807", string(resultMap["bigInt"].(json.Number)))
	assert.Equal(t, "123.456789", string(resultMap["float"].(json.Number)))
}

// TestFlattenToMap_SimpleObject_019 tests flattenToMap with simple objects
func TestFlattenToMap_SimpleObject_019(t *testing.T) {
	input := map[string]interface{}{
		"name": "John",
		"age":  json.Number("30"),
	}
	output := make(map[string]string)

	flattenToMap(input, "", output)

	expected := map[string]string{
		"$.age":  "30",
		"$.name": `"John"`,
	}
	assert.Equal(t, expected, output)
}

// TestFlattenToMap_NestedObject_020 tests flattenToMap with nested objects
func TestFlattenToMap_NestedObject_020(t *testing.T) {
	input := map[string]interface{}{
		"user": map[string]interface{}{
			"name": "John",
			"details": map[string]interface{}{
				"age":  json.Number("30"),
				"city": "NY",
			},
		},
	}
	output := make(map[string]string)

	flattenToMap(input, "", output)

	expected := map[string]string{
		"$.user.details.age":  "30",
		"$.user.details.city": `"NY"`,
		"$.user.name":         `"John"`,
	}
	assert.Equal(t, expected, output)
}

// TestFlattenToMap_Array_021 tests flattenToMap with arrays
func TestFlattenToMap_Array_021(t *testing.T) {
	input := []interface{}{
		"first",
		json.Number("42"),
		map[string]interface{}{
			"nested": "value",
		},
	}
	output := make(map[string]string)

	flattenToMap(input, "", output)

	expected := map[string]string{
		"$[0]":        `"first"`,
		"$[1]":        "42",
		"$[2].nested": `"value"`,
	}
	assert.Equal(t, expected, output)
}

// TestFlattenToMap_ArrayInObject_022 tests flattenToMap with arrays inside objects
func TestFlattenToMap_ArrayInObject_022(t *testing.T) {
	input := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"id":   json.Number("1"),
				"name": "item1",
			},
			map[string]interface{}{
				"id":   json.Number("2"),
				"name": "item2",
			},
		},
	}
	output := make(map[string]string)

	flattenToMap(input, "", output)

	expected := map[string]string{
		"$.items[0].id":   "1",
		"$.items[0].name": `"item1"`,
		"$.items[1].id":   "2",
		"$.items[1].name": `"item2"`,
	}
	assert.Equal(t, expected, output)
}

// TestPathWithDollar_023 tests pathWithDollar function
func TestPathWithDollar_023(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"", "$"},
		{"name", "$.name"},
		{"$.name", "$.name"},
		{"$[0]", "$[0]"},
		{"user.details.age", "$.user.details.age"},
	}

	for _, tc := range testCases {
		result := pathWithDollar(tc.input)
		assert.Equal(t, tc.expected, result, "Input: %s", tc.input)
	}
}

// TestGenerateTableDiff_SortedOutput_024 tests that output keys are sorted
func TestGenerateTableDiff_SortedOutput_024(t *testing.T) {
	expected := `{"z": "last", "a": "first", "m": "middle"}`
	actual := `{"z": "changed", "a": "changed", "m": "changed"}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)

	// Split by lines and find the order of Path entries
	lines := strings.Split(diff, "\n")
	pathLines := make([]string, 0)
	for _, line := range lines {
		if strings.HasPrefix(line, "Path: ") {
			pathLines = append(pathLines, line)
		}
	}

	// Should be sorted: a, m, z
	require.Len(t, pathLines, 3)
	assert.Contains(t, pathLines[0], "Path: a")
	assert.Contains(t, pathLines[1], "Path: m")
	assert.Contains(t, pathLines[2], "Path: z")
}

// TestGenerateTableDiff_JSONWithNulls_025 tests handling of null values
func TestGenerateTableDiff_JSONWithNulls_025(t *testing.T) {
	expected := `{"value": null, "other": "test"}`
	actual := `{"value": "changed", "other": null}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: other")
	assert.Contains(t, diff, `Expected: "test"`)
	assert.Contains(t, diff, "Actual: null")
	assert.Contains(t, diff, "Path: value")
	assert.Contains(t, diff, "Expected: null")
	assert.Contains(t, diff, `Actual: "changed"`)
}

// TestGenerateTableDiff_ComplexRealWorldExample_026 tests with a complex real-world-like JSON
func TestGenerateTableDiff_ComplexRealWorldExample_026(t *testing.T) {
	// Simplified version of the sample report data
	expected := `{
		"status": 200,
		"data": {
			"page_info": {
				"id": "99999",
				"name": "keploy improve"
			},
			"page_content": {
				"something": [
					{
						"id": 50000,
						"type": "Keploy_detail",
						"data": {
							"tabs": [
								{
									"id": "777",
									"name": "Bugs"
								}
							]
						}
					}
				]
			}
		}
	}`

	actual := `{
		"status": 200,
		"data": {
			"page_info": {
				"id": "13777",
				"name": "Updated tests"
			},
			"page_content": {
				"something": [
					{
						"id": 50000,
						"type": "Keploy_detail",
						"data": {
							"tabs": [
								{
									"id": "7891",
									"name": "Tests"
								}
							]
						}
					}
				]
			}
		}
	}`

	diff, err := GenerateTableDiff(expected, actual)

	require.NoError(t, err)
	assert.Contains(t, diff, "Path: data.page_content.something[0].data.tabs[0].id")
	assert.Contains(t, diff, `Expected: "777"`)
	assert.Contains(t, diff, `Actual: "7891"`)
	assert.Contains(t, diff, "Path: data.page_content.something[0].data.tabs[0].name")
	assert.Contains(t, diff, `Expected: "Bugs"`)
	assert.Contains(t, diff, `Actual: "Tests"`)
	assert.Contains(t, diff, "Path: data.page_info.id")
	assert.Contains(t, diff, `Expected: "99999"`)
	assert.Contains(t, diff, `Actual: "13777"`)
	assert.Contains(t, diff, "Path: data.page_info.name")
	assert.Contains(t, diff, `Expected: "keploy improve"`)
	assert.Contains(t, diff, `Actual: "Updated tests"`)
}

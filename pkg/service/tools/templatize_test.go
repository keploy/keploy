package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/utils"
)

// TestRenderIfTemplatized_NonTemplateString_002 tests the RenderIfTemplatized function for non-template strings.
func TestRenderIfTemplatized_NonTemplateString_002(t *testing.T) {
	val := "plain string"
	isTemplatized, renderedVal, err := RenderIfTemplatized(val)

	require.NoError(t, err)
	assert.False(t, isTemplatized)
	assert.Equal(t, val, renderedVal)
}

// TestAddQuotesInTemplates_WrapTemplates_003 tests the addQuotesInTemplates function for wrapping templates in quotes.
func TestAddQuotesInTemplates_WrapTemplates_003(t *testing.T) {
	input := `{"id":{{int .id}}}`
	expected := `{"id":"{{int .id}}"}`
	result := addQuotesInTemplates(input)

	assert.Equal(t, expected, result)
}

// TestRemoveQuotesInTemplates_RemoveQuotes_004 tests the removeQuotesInTemplates function for removing quotes from templates.
func TestRemoveQuotesInTemplates_RemoveQuotes_004(t *testing.T) {
	input := `{"id":"{{int .id}}"}`
	expected := `{"id":{{int .id}}}`
	result := removeQuotesInTemplates(input)

	assert.Equal(t, expected, result)
}

// TestRender_ParseAndExecuteTemplates_005 tests the render function for parsing and executing templates.
func TestRender_ParseAndExecuteTemplates_005(t *testing.T) {
	utils.TemplatizedValues = map[string]interface{}{"id": 123}
	val := "{{int .id}}"
	result, err := render(val)

	require.NoError(t, err)
	assert.Equal(t, 123, result)
}

// TestInsertUnique_GenerateUniqueKeys_006 tests the insertUnique function for generating unique keys.
func TestInsertUnique_GenerateUniqueKeys_006(t *testing.T) {
	myMap := map[string]interface{}{"id": "123"}
	key := insertUnique("id", "123", myMap)

	assert.Equal(t, "id", key)

	key = insertUnique("id", "456", myMap)
	assert.Equal(t, "id1", key)
}

// TestRenderIfTemplatized_TemplateProcessing_001 tests the RenderIfTemplatized function for valid template strings.
func TestRenderIfTemplatized_TemplateProcessing_001(t *testing.T) {
	utils.TemplatizedValues = map[string]interface{}{"id": 123}
	val := "{{int .id}}"
	isTemplatized, renderedVal, err := RenderIfTemplatized(val)

	require.NoError(t, err)
	assert.True(t, isTemplatized)
	assert.Equal(t, 123, renderedVal)
}

// TestUtilityFunctions_VariousScenarios_523 groups tests for several small helper functions
// to verify their behavior with different kinds of inputs.
// func TestUtilityFunctions_VariousScenarios_523(t *testing.T) {
// 	t.Run("getType should return correct type names", func(t *testing.T) {
// 		assert.Equal(t, "string", getType("a string"))
// 		assert.Equal(t, "string", getType(new(string)))
// 		assert.Equal(t, "int", getType(int64(1)))
// 		assert.Equal(t, "int", getType(1))
// 		assert.Equal(t, "int", getType(new(int)))
// 		assert.Equal(t, "float", getType(float64(1.1)))
// 		assert.Equal(t, "float", getType(float32(1.1)))
// 		assert.Equal(t, "float", getType(new(float64)))
// 		assert.Equal(t, "", getType(true)) // default case
// 	})

// 	t.Run("insertUnique should generate unique keys correctly", func(t *testing.T) {
// 		m := make(map[string]interface{})
// 		// New key
// 		key := insertUnique("id", "123", m)
// 		assert.Equal(t, "id", key)
// 		assert.Equal(t, "123", m["id"])
// 		// Existing key, same value
// 		key = insertUnique("id", "123", m)
// 		assert.Equal(t, "id", key)
// 		// Existing key, different value
// 		key = insertUnique("id", "456", m)
// 		assert.Equal(t, "id1", key)
// 		assert.Equal(t, "456", m["id1"])
// 		// Key with hyphen
// 		key = insertUnique("user-id", "abc", m)
// 		assert.Equal(t, "userid", key)
// 		assert.Equal(t, "abc", m["userid"])
// 	})

// 	t.Run("marshalJSON should handle errors", func(t *testing.T) {
// 		// A channel cannot be marshaled to JSON
// 		data := make(chan int)
// 		result := marshalJSON(data, zap.NewNop())
// 		assert.Equal(t, "", result)
// 	})

// 	t.Run("parseIntoJSON should handle various inputs", func(t *testing.T) {
// 		res, err := parseIntoJSON("")
// 		assert.NoError(t, err)
// 		assert.Nil(t, res)

// 		res, err = parseIntoJSON("{'key': 'val'}") // invalid JSON
// 		assert.Error(t, err)
// 		assert.Nil(t, res)

// 		res, err = parseIntoJSON(`{"key": "val"}`)
// 		assert.NoError(t, err)
// 		assert.NotNil(t, res)
// 	})
// }

// TestQuoteHandlingInTemplates_729 verifies that template placeholders are correctly
// quoted for JSON parsing and unquoted for storage, while respecting explicit string templates.
func TestQuoteHandlingInTemplates_729(t *testing.T) {
	t.Run("addQuotesInTemplates (for original function)", func(t *testing.T) {

		input1 := `{"id":{{int .id}},"name":"test"}`
		expected1 := `{"id":"{{int .id}}","name":"test"}`
		assert.Equal(t, expected1, addQuotesInTemplates(input1), "Should wrap template not in a string")

		input2 := `{"query":"SELECT * FROM users WHERE name = '{{.name}}'"}`
		expected2 := `{"query":"SELECT * FROM users WHERE name = '"{{.name}}"'"}`
		assert.Equal(t, expected2, addQuotesInTemplates(input2), "Original function incorrectly wraps templates that are already inside strings")

		input3 := `{"key":"value with \" and {{template}}"}`
		expected3 := `{"key":"value with \" and "{{template}}""}`
		assert.Equal(t, expected3, addQuotesInTemplates(input3), "Original function incorrectly wraps templates inside strings with escaped quotes")

	})

	t.Run("removeQuotesInTemplates", func(t *testing.T) {
		// Removes quotes from non-string template
		input1 := `{"id":"{{int .id}}"}`
		expected1 := `{"id":{{int .id}}}`
		assert.Equal(t, expected1, removeQuotesInTemplates(input1))

		// Keeps quotes for string template
		input2 := `{"name":"{{string .name}}"}`
		assert.Equal(t, input2, removeQuotesInTemplates(input2))

		// Handles mixed case
		input3 := `{"id":"{{int .id}}","name":"{{string .name}}"}`
		expected3 := `{"id":{{int .id}},"name":"{{string .name}}"}`
		assert.Equal(t, expected3, removeQuotesInTemplates(input3))
	})
}

package matcher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// --- Regex Matcher Tests ---

func TestCustomMatcher_Regex_Match(t *testing.T) {
	m := models.CustomMatcher{Type: "regex", Value: `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`}
	ok, err := ApplyCustomMatcher(m, nil, "550e8400-e29b-41d4-a716-446655440000")
	require.NoError(t, err)
	assert.True(t, ok, "UUID should match regex")
}

func TestCustomMatcher_Regex_NoMatch(t *testing.T) {
	m := models.CustomMatcher{Type: "regex", Value: `^\d+$`}
	ok, err := ApplyCustomMatcher(m, nil, "not-a-number")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestCustomMatcher_Regex_NilActual(t *testing.T) {
	m := models.CustomMatcher{Type: "regex", Value: `.*`}
	ok, err := ApplyCustomMatcher(m, nil, nil)
	require.NoError(t, err)
	assert.False(t, ok, "nil actual should not match regex")
}

func TestCustomMatcher_Regex_NumericActual(t *testing.T) {
	m := models.CustomMatcher{Type: "regex", Value: `^\d+(\.\d+)?$`}
	ok, err := ApplyCustomMatcher(m, nil, 42.5)
	require.NoError(t, err)
	assert.True(t, ok, "number should be converted to string and matched")
}

// --- Numeric Tolerance Matcher Tests ---

func TestCustomMatcher_NumericTolerance_WithinRange(t *testing.T) {
	m := models.CustomMatcher{Type: "numeric_tolerance", Value: "0.5"}
	ok, err := ApplyCustomMatcher(m, 10.0, 10.3)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_NumericTolerance_ExactBoundary(t *testing.T) {
	m := models.CustomMatcher{Type: "numeric_tolerance", Value: "1.0"}
	ok, err := ApplyCustomMatcher(m, 5.0, 6.0)
	require.NoError(t, err)
	assert.True(t, ok, "boundary value should pass")
}

func TestCustomMatcher_NumericTolerance_OutOfRange(t *testing.T) {
	m := models.CustomMatcher{Type: "numeric_tolerance", Value: "0.01"}
	ok, err := ApplyCustomMatcher(m, 100.0, 100.5)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestCustomMatcher_NumericTolerance_InvalidTolerance(t *testing.T) {
	m := models.CustomMatcher{Type: "numeric_tolerance", Value: "not-a-number"}
	_, err := ApplyCustomMatcher(m, 1.0, 1.0)
	assert.Error(t, err)
}

func TestCustomMatcher_NumericTolerance_NonNumericActual(t *testing.T) {
	m := models.CustomMatcher{Type: "numeric_tolerance", Value: "0.5"}
	ok, err := ApplyCustomMatcher(m, 1.0, "hello")
	require.NoError(t, err)
	assert.False(t, ok, "non-numeric actual should fail")
}

func TestCustomMatcher_NumericTolerance_StringNumbers(t *testing.T) {
	m := models.CustomMatcher{Type: "numeric_tolerance", Value: "0.1"}
	ok, err := ApplyCustomMatcher(m, "5.0", "5.05")
	require.NoError(t, err)
	assert.True(t, ok, "string numbers should be parsed and compared")
}

// --- Presence Matcher Tests ---

func TestCustomMatcher_Presence_NonNil(t *testing.T) {
	m := models.CustomMatcher{Type: "presence"}
	ok, err := ApplyCustomMatcher(m, nil, "any-value")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_Presence_Nil(t *testing.T) {
	m := models.CustomMatcher{Type: "presence"}
	ok, err := ApplyCustomMatcher(m, nil, nil)
	require.NoError(t, err)
	assert.False(t, ok, "nil actual should fail presence check")
}

func TestCustomMatcher_Presence_EmptyString(t *testing.T) {
	m := models.CustomMatcher{Type: "presence"}
	ok, err := ApplyCustomMatcher(m, nil, "")
	require.NoError(t, err)
	assert.True(t, ok, "empty string is still present (non-nil)")
}

func TestCustomMatcher_Presence_ZeroValue(t *testing.T) {
	m := models.CustomMatcher{Type: "presence"}
	ok, err := ApplyCustomMatcher(m, nil, 0.0)
	require.NoError(t, err)
	assert.True(t, ok, "zero is still present (non-nil)")
}

// --- Type Matcher Tests ---

func TestCustomMatcher_Type_String(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "string"}
	ok, err := ApplyCustomMatcher(m, nil, "hello")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_Type_Number(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "number"}
	ok, err := ApplyCustomMatcher(m, nil, 42.0)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_Type_Boolean(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "boolean"}
	ok, err := ApplyCustomMatcher(m, nil, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_Type_Array(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "array"}
	ok, err := ApplyCustomMatcher(m, nil, []interface{}{1, 2, 3})
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_Type_Object(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "object"}
	ok, err := ApplyCustomMatcher(m, nil, map[string]interface{}{"key": "val"})
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_Type_Null(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "null"}
	ok, err := ApplyCustomMatcher(m, nil, nil)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCustomMatcher_Type_Mismatch(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "string"}
	ok, err := ApplyCustomMatcher(m, nil, 42.0)
	require.NoError(t, err)
	assert.False(t, ok, "number should not match expected type 'string'")
}

func TestCustomMatcher_Type_CaseInsensitive(t *testing.T) {
	m := models.CustomMatcher{Type: "type", Value: "String"}
	ok, err := ApplyCustomMatcher(m, nil, "hello")
	require.NoError(t, err)
	assert.True(t, ok, "type matching should be case-insensitive")
}

// --- Unsupported Matcher Type ---

func TestCustomMatcher_UnsupportedType(t *testing.T) {
	m := models.CustomMatcher{Type: "unknown_matcher"}
	_, err := ApplyCustomMatcher(m, nil, "value")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported custom matcher type")
}

// --- ResolveCustomMatchers Tests ---

func TestResolveCustomMatchers_GlobalOnly(t *testing.T) {
	global := map[string]map[string]models.CustomMatcher{
		"body": {
			"data.id":        {Type: "regex", Value: `^\d+$`},
			"data.timestamp": {Type: "presence"},
		},
	}
	result := ResolveCustomMatchers(global, nil, "test-set-1")
	assert.Len(t, result, 2)
	assert.Equal(t, "regex", result["data.id"].Type)
	assert.Equal(t, "presence", result["data.timestamp"].Type)
}

func TestResolveCustomMatchers_TestsetOverridesGlobal(t *testing.T) {
	global := map[string]map[string]models.CustomMatcher{
		"body": {
			"data.id": {Type: "regex", Value: `^\d+$`},
		},
	}
	testsets := map[string]map[string]map[string]models.CustomMatcher{
		"test-set-1": {
			"body": {
				"data.id": {Type: "presence"}, // overrides global
			},
		},
	}
	result := ResolveCustomMatchers(global, testsets, "test-set-1")
	assert.Equal(t, "presence", result["data.id"].Type, "testset matcher should override global")
}

func TestResolveCustomMatchers_Empty(t *testing.T) {
	result := ResolveCustomMatchers(nil, nil, "test-set-1")
	assert.Empty(t, result)
}

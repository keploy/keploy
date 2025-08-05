package http

import (
	"testing"

	"net/http"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// TestMatch_JSONComparisonWithNoiseControl_123 tests the behavior of the Match function when comparing JSON responses with noise control.
func TestMatch_JSONComparisonWithNoiseControl_123(t *testing.T) {
	// Arrange
	mockLogger := zap.NewNop()
	tc := &models.TestCase{
		Name: "TestCase1",
		HTTPResp: models.HTTPResp{
			Body:       `{"key1":"value1","key2":"value2"}`,
			StatusCode: 200,
		},
		Noise: map[string][]string{
			"body.key2": {"value2"},
		},
	}
	actualResponse := &models.HTTPResp{
		Body:       `{"key1":"value1","key2":"value2"}`,
		StatusCode: 200,
	}
	noiseConfig := map[string]map[string][]string{
		"body": {
			"key2": {"value2"},
		},
	}
	ignoreOrdering := false

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, mockLogger)

	// Assert
	require.NotNil(t, result)
	assert.True(t, pass)
	assert.True(t, result.BodyResult[0].Normal)
	assert.Equal(t, tc.HTTPResp.StatusCode, result.StatusCode.Expected)
	assert.Equal(t, actualResponse.StatusCode, result.StatusCode.Actual)
}

// TestAssertionMatch_AssertionsProvided_456 tests the behavior of the AssertionMatch function when assertions are provided.
func TestAssertionMatch_AssertionsProvided_456(t *testing.T) {
	// Arrange
	mockLogger := zap.NewNop()
	tc := &models.TestCase{
		Assertions: map[models.AssertionType]interface{}{
			models.StatusCode: 200,
			models.JsonEqual:  `{"key":"value"}`,
		},
		HTTPResp: models.HTTPResp{
			Body:       `{"key":"value"}`,
			StatusCode: 200,
		},
	}
	actualResponse := &models.HTTPResp{
		Body:       `{"key":"value"}`,
		StatusCode: 200,
	}

	// Act
	pass, result := AssertionMatch(tc, actualResponse, mockLogger)

	// Assert
	require.NotNil(t, result)
	assert.True(t, pass)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestFlattenHTTPResponse_HeadersAndBodyProvided_789 tests the behavior of the FlattenHTTPResponse function when headers and body are provided.
func TestFlattenHTTPResponse_HeadersAndBodyProvided_789(t *testing.T) {
	// Arrange
	headers := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer token"},
	}
	body := `{"key":"value"}`

	// Act
	result, err := FlattenHTTPResponse(headers, body)

	// Assert
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, []string{"application/json"}, result["header.Content-Type"])
	assert.Equal(t, []string{"Bearer token"}, result["header.Authorization"])
	assert.Contains(t, result, "body.key")
}

// TestMatch_JSONComparisonWithMismatchedNestedStructures_654 tests the behavior of the Match function when JSON responses have mismatched nested structures.
func TestMatch_JSONComparisonWithMismatchedNestedStructures_654(t *testing.T) {
	// Arrange
	mockLogger := zap.NewNop()
	tc := &models.TestCase{
		Name: "TestCaseMismatchedNested",
		HTTPResp: models.HTTPResp{
			Body:       `{"key1":"value1","key2":{"nestedKey":"expectedValue"}}`,
			StatusCode: 200,
		},
		Noise: map[string][]string{},
	}
	actualResponse := &models.HTTPResp{
		Body:       `{"key1":"value1","key2":{"nestedKey":"actualValue"}}`,
		StatusCode: 200,
	}
	noiseConfig := map[string]map[string][]string{}
	ignoreOrdering := false

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, mockLogger)

	// Assert
	require.NotNil(t, result)
	assert.False(t, pass)
	assert.False(t, result.BodyResult[0].Normal)
	assert.Equal(t, tc.HTTPResp.StatusCode, result.StatusCode.Expected)
	assert.Equal(t, actualResponse.StatusCode, result.StatusCode.Actual)
}

// TestMatch_BodyMismatchWithNestedJson_101 tests the diffing logic for nested JSON strings within a response body.
func TestMatch_BodyMismatchWithNestedJson_101(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-nested-json-diff",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json"},
			Body:       `{"data": "{\"id\": \"id-123\"}"}`,
		},
		Noise: map[string][]string{},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       `{"data": "{\"id\": \"id-456\"}"}`,
	}
	noiseConfig := map[string]map[string][]string{}

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, false, logger)

	// Assert
	assert.False(t, pass, "Match should fail for different nested JSON strings")
	require.NotNil(t, result)
	assert.False(t, result.BodyResult[0].Normal)
}

// TestMatch_StatusCodeMismatch_111 verifies that a status code mismatch results in a failed match.
func TestMatch_StatusCodeMismatch_111(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-status-mismatch",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       "ok",
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 500,
		Body:       "ok",
	}
	noiseConfig := map[string]map[string][]string{}

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, false, logger)

	// Assert
	assert.False(t, pass)
	require.NotNil(t, result)
	assert.False(t, result.StatusCode.Normal)
	assert.Equal(t, 200, result.StatusCode.Expected)
	assert.Equal(t, 500, result.StatusCode.Actual)
}

// TestMatch_WithAssertionsDelegation_222 ensures that if assertions are provided, they take precedence over standard matching.
func TestMatch_WithAssertionsDelegation_222(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-with-assertions",
		HTTPResp: models.HTTPResp{
			StatusCode: 500,         // Mismatch
			Body:       "different", // Mismatch
		},
		Assertions: map[models.AssertionType]interface{}{
			models.StatusCode: 200, // This should be used for matching
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200, // Match with assertion
		Body:       "body",
	}
	noiseConfig := map[string]map[string][]string{}

	// Act
	// The normal match would fail, but because assertions are present, AssertionMatch is called.
	pass, result := Match(tc, actualResponse, noiseConfig, false, logger)

	// Assert
	assert.True(t, pass) // Assertion should pass
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_BodyNoiseWildcardAll_901 verifies that when a wildcard noise rule `"*": ["*"]`
// is present for the body, the body comparison is effectively skipped, leading to a pass
// even if the bodies are different.
func TestMatch_BodyNoiseWildcardAll_901(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-wildcard-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"message": "hello"}`,
		},
		Noise: map[string][]string{},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"message": "world"}`,
	}
	noiseConfig := map[string]map[string][]string{
		"body": {"*": {"*"}},
	}

	pass, result := Match(tc, actualResponse, noiseConfig, false, logger)

	assert.True(t, pass, "Match should pass because of wildcard body noise")
	require.NotNil(t, result)
	assert.True(t, result.BodyResult[0].Normal, "BodyResult should be normal due to noise config")
}

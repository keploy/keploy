package http

import (
	"testing"

	"errors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestMatch_HeaderNoiseUpdate_123 ensures that the `headerNoise` map is updated correctly when the `noise` map contains a "header" key.
func TestMatch_HeaderNoiseUpdate_123(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	tc := &models.TestCase{
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json"},
			Body:       `{"key":"value"}`,
		},
		Noise: map[string][]string{
			"header.Content-Type": {"regex"},
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       `{"key":"value"}`,
	}
	noiseConfig := map[string]map[string][]string{
		"header": {},
	}
	ignoreOrdering := false

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, logger)

	// Assert
	require.NotNil(t, result)
	assert.True(t, pass)
	assert.Contains(t, noiseConfig["header"], "content-type")
	assert.Equal(t, []string{"regex"}, noiseConfig["header"]["content-type"])
}

// TestMatch_FailureAndDiffLogging_890 tests the Match function with comprehensive failures
// in status code, headers, and JSON body to ensure that the diff logging mechanism is triggered.
func TestMatch_FailureAndDiffLogging_890(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-comprehensive-fail",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Expected-Header": "value1"},
			Body:       `{"id": 1, "value": "expected"}`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 404, // Mismatch
		Header:     map[string]string{"Actual-Header": "value2"},
		Body:       `{"id": 2, "value": "actual"}`, // Mismatch
	}
	noiseConfig := map[string]map[string][]string{}
	ignoreOrdering := false

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, logger)

	// Assert
	assert.False(t, pass, "Should fail due to multiple mismatches")
	require.NotNil(t, result)
	assert.False(t, result.StatusCode.Normal)
	assert.False(t, result.BodyResult[0].Normal)
	// We can't easily assert the console output, but by running this
	// we exercise the entire diff generation logic in lines 121-301.
}

// TestMatch_BodyNoiseFromTestCase_124 verifies that the Match function correctly applies
// noise rules defined within the TestCase's Noise field to ignore specific JSON body fields.
func TestMatch_BodyNoiseFromTestCase_124(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-body-noise-from-tc",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"id": 123, "name": "expected"}`,
		},
		Noise: map[string][]string{
			"body.id": {".*"}, // Ignore the 'id' field
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"id": 456, "name": "expected"}`, // Only 'id' is different
	}
	noiseConfig := map[string]map[string][]string{}
	ignoreOrdering := false

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, logger)

	// Assert
	assert.True(t, pass, "Should pass because the 'id' field difference is covered by noise")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_RedirectToAssertionMatch_567 ensures that if a TestCase contains assertions,
// the Match function correctly calls AssertionMatch and returns its result.
func TestMatch_RedirectToAssertionMatch_567(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-redirect-to-assertion",
		HTTPResp: models.HTTPResp{
			StatusCode: 201, // Deliberate mismatch to show normal matching would fail
			Body:       `{"key":"wrong"}`,
		},
		Assertions: map[models.AssertionType]interface{}{
			models.StatusCode: 200,
			models.JsonContains: map[string]interface{}{
				"key": "value",
			},
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"key":"value", "other": "stuff"}`,
	}
	noiseConfig := map[string]map[string][]string{}
	ignoreOrdering := false

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, logger)

	// Assert
	assert.True(t, pass, "AssertionMatch should be called and return true")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_InvalidJSONBody_321 ensures that when the actual response body is not valid JSON,
// it is treated as plain text and compared directly, leading to a mismatch if different.
func TestMatch_InvalidJSONBody_321(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"id": "123", "name": "keploy"}`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"id": "123", "name": "keploy"`, // Invalid JSON
	}
	noiseConfig := map[string]map[string][]string{}

	pass, res := Match(tc, actualResponse, noiseConfig, false, logger)

	assert.False(t, pass)
	assert.False(t, res.BodyResult[0].Normal)
	assert.Equal(t, models.Plain, res.BodyResult[0].Type)
}

// TestMatch_JsonMarshalErrorInDiff_987 simulates a failure in json.Marshal when generating
// diffs for a failed test case to ensure the error is handled gracefully.
func TestMatch_JsonMarshalErrorInDiff_987(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-marshal-error",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"id": 1, "value": "expected"}`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"id": 1, "value": "actual"}`,
	}
	noiseConfig := map[string]map[string][]string{}

	originalJSONMarshal := jsonMarshal234
	jsonMarshal234 = func(v interface{}) ([]byte, error) {
		// This mock will fail the first time json.Marshal is called within the diffing logic.
		return nil, errors.New("mock marshal error")
	}
	defer func() { jsonMarshal234 = originalJSONMarshal }()

	pass, res := Match(tc, actualResponse, noiseConfig, false, logger)

	// The function returns (false, nil) on this specific error path
	assert.False(t, pass)
	assert.Nil(t, res)
}

// TestMatch_BodyNoiseWildcard_789 tests the scenario where a global noise configuration
// specifies that the entire body should be ignored ("*": "*"). Even if the actual
// response body is completely different from the expected one, the match should pass.
// It also verifies that the test case's noise map for the body is initialized.
func TestMatch_BodyNoiseWildcard_789(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-wildcard-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"id": 1, "name": "keploy"}`,
		},
		Noise: map[string][]string{}, // Noise is empty in TC
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"id": 2, "name": "keploy-test"}`, // Body is completely different
	}
	// Global noise config says to ignore the entire body
	noiseConfig := map[string]map[string][]string{
		"body": {"*": {"*"}},
	}
	ignoreOrdering := false

	// Act
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, logger)

	// Assert
	assert.True(t, pass, "Should pass because the entire body is ignored by wildcard noise")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
	// Check that tc.Noise["body"] was initialized
	assert.NotNil(t, tc.Noise["body"])
}

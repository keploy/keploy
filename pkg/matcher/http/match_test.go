package http

import (
	"strings"
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
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, false, logger, true)

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
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, false, logger, true)

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
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, false, logger, true)

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
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, false, logger, true)

	// Assert
	assert.True(t, pass, "AssertionMatch should be called and return true")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_InvalidJSONBody_321 ensures that when the actual response body is not valid JSON,
// it is treated as plain text and compared directly, leading to a mismatch if different.
// TestMatch_InvalidJSONBody_321 ensures that when the actual response body is not valid JSON,
// it is treated as plain text and compared directly, leading to a mismatch if different.
// This test uses compareAll=true to ensure body comparison happens.
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

	// compareAll=true ensures non-JSON bodies are compared
	pass, res := Match(tc, actualResponse, noiseConfig, false, true, logger, true)

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

	pass, res := Match(tc, actualResponse, noiseConfig, false, false, logger, true)

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
	pass, result := Match(tc, actualResponse, noiseConfig, ignoreOrdering, false, logger, true)

	// Assert
	assert.True(t, pass, "Should pass because the entire body is ignored by wildcard noise")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
	// Check that tc.Noise["body"] was initialized
	assert.NotNil(t, tc.Noise["body"])
}

// TestMatch_CompareAll_Disabled tests that when compareAll is false (default),
// non-JSON body differences are ignored and the match passes.
func TestMatch_CompareAll_Disabled(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-compare-all-disabled",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `<html><body>Expected HTML Content</body></html>`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `<html><body>Different HTML Content</body></html>`, // Different HTML
	}
	noiseConfig := map[string]map[string][]string{}

	// Act - with compareAll disabled (default behavior - skip non-JSON body comparison)
	pass, result := Match(tc, actualResponse, noiseConfig, false, false, logger, true)

	// Assert - should pass because non-JSON body comparison is skipped when compareAll is false
	assert.True(t, pass, "Should pass because compareAll is false and body is not JSON")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_CompareAll_Enabled tests that when compareAll is true,
// non-JSON body differences cause the match to fail.
func TestMatch_CompareAll_Enabled(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-compare-all-enabled",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `<html><body>Expected HTML Content</body></html>`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `<html><body>Different HTML Content</body></html>`, // Different HTML
	}
	noiseConfig := map[string]map[string][]string{}

	// Act - with compareAll enabled (compare all body types)
	pass, result := Match(tc, actualResponse, noiseConfig, false, true, logger, true)

	// Assert - should fail because compareAll is enabled and bodies differ
	assert.False(t, pass, "Should fail because compareAll is true and body differs")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.False(t, result.BodyResult[0].Normal)
}

// TestMatch_CompareAll_JSONStillCompared tests that when compareAll is false,
// JSON body comparison still happens normally (only non-JSON is skipped).
func TestMatch_CompareAll_JSONStillCompared(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-json-still-compared",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"id": 1, "name": "expected"}`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"id": 2, "name": "actual"}`, // Different JSON
	}
	noiseConfig := map[string]map[string][]string{}

	// Act - with compareAll disabled, but body is JSON
	pass, result := Match(tc, actualResponse, noiseConfig, false, false, logger, true)

	// Assert - should fail because JSON bodies are still compared even with compareAll disabled
	assert.False(t, pass, "Should fail because JSON bodies are different")
	require.NotNil(t, result)
	assert.True(t, result.StatusCode.Normal)
	assert.False(t, result.BodyResult[0].Normal)
}

// ── Level 2: Match() Integration Tests ───────────────────────────────────────

// TestMatch_HTML_SemanticMatch verifies that two HTML bodies that differ only in
// whitespace and attribute ordering are treated as equal via CanonicalizeHTML.
// NOTE: Bodies must begin with an HTML tag AND contain a void element like <br>
// so that IsXML() returns false and DetectContentType() classifies them as text/html.
// Pure XHTML like <html><body>text</body></html> is valid XML and would be mis-classified.
func TestMatch_HTML_SemanticMatch(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-html-semantic-match",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			// <br> is a void element — makes this invalid XML, so IsXML returns false
			// and GuessContentType correctly returns models.HTML.
			Body: `<html><body><br><div class="a" id="1">Hello   World</div></body></html>`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		// Same document — attributes reordered + whitespace collapsed — semantically identical.
		Body: `<html><body><br><div id="1" class="a">Hello World</div></body></html>`,
	}
	noiseConfig := map[string]map[string][]string{}

	pass, result := Match(tc, actualResponse, noiseConfig, false, true, logger, false)

	assert.True(t, pass, "semantically identical HTML should pass")
	require.NotNil(t, result)
	assert.True(t, result.BodyResult[0].Normal)
}


// TestMatch_HTML_DifferentContent verifies that HTML bodies with genuinely different
// text content fail comparison even with compareAll=true.
func TestMatch_HTML_DifferentContent(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-html-different-content",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `<html><body><br><h1>Expected</h1></body></html>`,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `<html><body><br><h1>Different</h1></body></html>`,
	}
	noiseConfig := map[string]map[string][]string{}

	pass, result := Match(tc, actualResponse, noiseConfig, false, true, logger, false)

	assert.False(t, pass, "HTML with different text content must fail")
	require.NotNil(t, result)
	assert.False(t, result.BodyResult[0].Normal)
}

// TestMatch_HTML_TypeMismatch verifies that when the expected body is plain text
// but the actual body is HTML, the implementation falls back to raw string comparison
// (no false positive from html.Parse wrapping both in an identical skeleton).
func TestMatch_HTML_TypeMismatch(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-html-type-mismatch",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `Hello World`, // plain text
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `<html><body>Hello World</body></html>`, // HTML
	}
	noiseConfig := map[string]map[string][]string{}

	pass, result := Match(tc, actualResponse, noiseConfig, false, true, logger, false)

	// Different raw strings → must fail (raw fallback, not HTML canonicalization).
	assert.False(t, pass, "plain-text expected vs HTML actual must not produce a false positive")
	require.NotNil(t, result)
	assert.False(t, result.BodyResult[0].Normal)
}

// TestMatch_HTML_SizeGuard verifies that when the HTML body exceeds 1MB, the
// canonicalization error is handled gracefully and raw string comparison takes over.
func TestMatch_HTML_SizeGuard(t *testing.T) {
	logger := zap.NewNop()

	// 3 MB of valid-looking HTML — above the 1MB guard in CanonicalizeHTML.
	bigHTML := `<html><body>` + strings.Repeat(`<div>test</div>`, 200_000) + `</body></html>`

	tc := &models.TestCase{
		Name: "test-html-size-guard",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       bigHTML,
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       bigHTML, // identical — raw comparison must still pass
	}
	noiseConfig := map[string]map[string][]string{}

	pass, result := Match(tc, actualResponse, noiseConfig, false, true, logger, false)

	// Bodies are identical strings — even with the canonicalization error,
	// the raw fallback `tc.HTTPResp.Body != actualResponse.Body` is false → pass.
	assert.True(t, pass, "identical oversized HTML should pass via raw string fallback")
	require.NotNil(t, result)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_JSON_Regression ensures the JSON comparison path still works exactly
// as before — HTML integration must not disturb it.
func TestMatch_JSON_HTMLIntegration_Regression(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-json-regression",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"status": "ok", "count": 42}`,
		},
	}

	t.Run("identical JSON passes", func(t *testing.T) {
		actual := &models.HTTPResp{
			StatusCode: 200,
			Body:       `{"status": "ok", "count": 42}`,
		}
		pass, result := Match(tc, actual, map[string]map[string][]string{}, false, false, logger, false)
		assert.True(t, pass)
		require.NotNil(t, result)
		assert.True(t, result.BodyResult[0].Normal)
	})

	t.Run("different JSON fails", func(t *testing.T) {
		actual := &models.HTTPResp{
			StatusCode: 200,
			Body:       `{"status": "ok", "count": 99}`,
		}
		pass, result := Match(tc, actual, map[string]map[string][]string{}, false, false, logger, false)
		assert.False(t, pass)
		require.NotNil(t, result)
		assert.False(t, result.BodyResult[0].Normal)
	})

	t.Run("noised JSON field passes despite difference", func(t *testing.T) {
		actual := &models.HTTPResp{
			StatusCode: 200,
			Body:       `{"status": "ok", "count": 999}`,
		}
		noiseConfig := map[string]map[string][]string{
			"body": {"count": {".*"}},
		}
		pass, result := Match(tc, actual, noiseConfig, false, false, logger, false)
		assert.True(t, pass, "noised field difference should be ignored")
		require.NotNil(t, result)
		assert.True(t, result.BodyResult[0].Normal)
	})
}

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

// TestMatch_HeaderNoiseUpdate_123 ensures a test case's "header.<name>" noise is
// applied when comparing headers.
//
// This used to assert that Match wrote the merged entry back into the caller's
// noiseConfig map. That was only observable because Match merged into the
// caller's map in place, which let one test case's noise leak into every later
// one in the same run. Match now merges into a copy, so the merge is asserted
// through its effect on the comparison instead, and the absence of the leak is
// asserted explicitly.
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
			"header.Content-Type": {".*"},
		},
	}
	// Content-Type differs; the test case's header noise must absorb it.
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/plain"},
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
	assert.True(t, pass, "the differing Content-Type is covered by the test case's header noise")
	assert.Empty(t, noiseConfig["header"], "the caller's noise config must not be mutated")
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
	// Match used to record the wildcard by writing a sentinel into tc.Noise.
	// That mutated the caller's test case (and panicked when tc.Noise was nil),
	// so the wildcard is now tracked locally and tc.Noise is left alone.
	assert.NotContains(t, tc.Noise, "body")
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

// The tests below cover the "sectioned" noise shape, where the key is the bare
// section name and the value lists the field paths to ignore:
//
//	noise:
//	  body:
//	    - items.product.stock
//	  header:
//	    - X-Request-Id
//
// This is the shape recorded test cases carry. It has
// to mean "ignore these paths", not "ignore this whole section" — the bare key
// only means whole-section when its list is empty (the wildcard sentinel that
// TestMatch_BodyNoiseWildcard_789 covers).

// TestMatch_SectionedBodyNoise_DoesNotDisableWholeBody is a regression test for
// a silent false negative: a sectioned body-noise entry disabled the body
// assertion entirely, so a test case with any auto-detected noisy field passed
// no matter what the handler returned.
func TestMatch_SectionedBodyNoise_DoesNotDisableWholeBody(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-sectioned-body-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"items":[{"product":{"name":"Laptop","price":1200,"stock":99}}]}`,
		},
		Noise: map[string][]string{
			"body": {"items.product.stock"},
		},
	}
	// stock is noise, but name and price are real regressions.
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"items":[{"product":{"name":"Tablet","price":1999,"stock":100}}]}`,
	}

	pass, result := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, true)

	assert.False(t, pass, "should fail: name and price differ and are not listed as noise")
	require.NotNil(t, result)
	assert.False(t, result.BodyResult[0].Normal)
}

// TestMatch_SectionedBodyNoise_IgnoresListedPaths is the other half of the
// contract: the paths that ARE listed must still be ignored.
func TestMatch_SectionedBodyNoise_IgnoresListedPaths(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-sectioned-body-noise-ignored",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"items":[{"product":{"name":"Laptop","price":1200,"stock":99}}]}`,
		},
		Noise: map[string][]string{
			"body": {"items.product.stock"},
		},
	}
	// Only the noisy field differs.
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"items":[{"product":{"name":"Laptop","price":1200,"stock":100}}]}`,
	}

	pass, result := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, true)

	assert.True(t, pass, "should pass: the only difference is the listed noisy path")
	require.NotNil(t, result)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_SectionedBodyNoise_MixedWithDottedKeys covers the shape a real
// recorded test case has, where both key styles appear together.
func TestMatch_SectionedBodyNoise_MixedWithDottedKeys(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-sectioned-and-dotted-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"created_at":"2026-08-05T08:22:51Z","stock":99,"name":"Laptop"}`,
		},
		Noise: map[string][]string{
			"body":            {"stock"},
			"body.created_at": {},
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"created_at":"2027-01-01T00:00:00Z","stock":100,"name":"Tablet"}`,
	}

	pass, result := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, true)

	assert.False(t, pass, "should fail on name; created_at and stock are both noise")
	require.NotNil(t, result)
	assert.False(t, result.BodyResult[0].Normal)
}

// TestMatch_EmptySectionedBodyNoise_IgnoresWholeBody pins the sentinel meaning
// of the bare key with an empty list, so the fix above cannot regress it.
func TestMatch_EmptySectionedBodyNoise_IgnoresWholeBody(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-empty-sectioned-body-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"id":1,"name":"keploy"}`,
		},
		Noise: map[string][]string{
			"body": {},
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"id":2,"name":"totally-different"}`,
	}

	pass, result := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, true)

	assert.True(t, pass, "an empty list on the bare key still means ignore the whole body")
	require.NotNil(t, result)
	assert.True(t, result.BodyResult[0].Normal)
}

// TestMatch_SectionedHeaderNoise_DoesNotDisableAllHeaders is the header analog
// of the body regression: `header: [X-Request-Id]` silenced every header.
func TestMatch_SectionedHeaderNoise_DoesNotDisableAllHeaders(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-sectioned-header-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header: map[string]string{
				"X-Request-Id": "abc",
				"Content-Type": "application/json",
			},
			Body: `{"ok":true}`,
		},
		Noise: map[string][]string{
			"header": {"X-Request-Id"},
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"X-Request-Id": "zzz",        // noise
			"Content-Type": "text/plain", // real regression
		},
		Body: `{"ok":true}`,
	}

	pass, result := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, true)

	assert.False(t, pass, "should fail: Content-Type differs and is not listed as noise")
	require.NotNil(t, result)
}

// TestMatch_SectionedHeaderNoise_IgnoresListedHeaders is the other half.
func TestMatch_SectionedHeaderNoise_IgnoresListedHeaders(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-sectioned-header-noise-ignored",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header: map[string]string{
				"X-Request-Id": "abc",
				"Content-Type": "application/json",
			},
			Body: `{"ok":true}`,
		},
		Noise: map[string][]string{
			"header": {"X-Request-Id"},
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"X-Request-Id": "zzz",
			"Content-Type": "application/json",
		},
		Body: `{"ok":true}`,
	}

	pass, result := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, true)

	assert.True(t, pass, "should pass: the only differing header is listed as noise")
	require.NotNil(t, result)
}

// Real payloads captured from a keploy cloud replay of order-service-4,
// test case get-api-v1-orders-by-id-details-1.
const realWorldRecordedBody = `{"created_at":"2026-08-05T08:22:51Z","id":"5978a5ae-792b-4a19-a97b-d2802674b467","items":[{"product":{"description":"A powerful and portable laptop.","id":"26c7d38d-8b7d-11f1-a6c8-da00f699ab49","name":"Laptop","price":1200,"stock":99},"productId":"26c7d38d-8b7d-11f1-a6c8-da00f699ab49","quantity":1}],"status":"PENDING","total_amount":1200,"updated_at":"2026-08-05T08:22:51Z","user":{"created_at":"2026-08-05T08:22:50Z","email":"alice_1785918170@example.com","id":"669e858c-b6ee-4313-b59d-f6fa1f51e33f","username":"alice_1785918170"},"userId":"669e858c-b6ee-4313-b59d-f6fa1f51e33f"}`

// Untampered replay: stock drifts 99 -> 100 (the field marked as noise).
const realWorldUntamperedBody = `{"created_at":"2026-08-05T08:22:51Z","id":"5978a5ae-792b-4a19-a97b-d2802674b467","items":[{"product":{"description":"A powerful and portable laptop.","id":"26c7d38d-8b7d-11f1-a6c8-da00f699ab49","name":"Laptop","price":1200,"stock":100},"productId":"26c7d38d-8b7d-11f1-a6c8-da00f699ab49","quantity":1}],"status":"PENDING","total_amount":1200,"updated_at":"2026-08-05T08:22:51Z","user":{"created_at":"2026-08-05T08:22:50Z","email":"alice_1785918170@example.com","id":"669e858c-b6ee-4313-b59d-f6fa1f51e33f","username":"alice_1785918170"},"userId":"669e858c-b6ee-4313-b59d-f6fa1f51e33f"}`

// Tampered replay: product mock returned Tablet/1999, user mock returned zzzzz.
const realWorldTamperedBody = `{"created_at":"2026-08-05T08:22:51Z","id":"5978a5ae-792b-4a19-a97b-d2802674b467","items":[{"product":{"description":"A powerful and portable laptop.","id":"26c7d38d-8b7d-11f1-a6c8-da00f699ab49","name":"Tablet","price":1999,"stock":100},"productId":"26c7d38d-8b7d-11f1-a6c8-da00f699ab49","quantity":1}],"status":"PENDING","total_amount":1200,"updated_at":"2026-08-05T08:22:51Z","user":{"created_at":"2026-08-05T08:22:50Z","email":"zzzzz_1785918170@example.com","id":"669e858c-b6ee-4313-b59d-f6fa1f51e33f","username":"zzzzz_1785918170"},"userId":"669e858c-b6ee-4313-b59d-f6fa1f51e33f"}`

func realWorldNoise() map[string][]string {
	return map[string][]string{
		"body":                 {"items.product.stock"},
		"body.created_at":      {},
		"body.updated_at":      {},
		"body.user.created_at": {},
		"header":               {"X-Request-Id"},
		"header.Date":          {},
	}
}

func TestMatch_RealWorldRecordedNoise(t *testing.T) {
	for _, c := range []struct {
		name     string
		actual   string
		wantPass bool
	}{
		{"untampered replay passes (only the noisy stock drifts)", realWorldUntamperedBody, true},
		{"tampered downstream mock is caught", realWorldTamperedBody, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{
				Name: "get-api-v1-orders-by-id-details-1",
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
					Header: map[string]string{
						"Content-Type": "application/json; charset=utf-8",
						"Date":         "Wed, 05 Aug 2026 08:22:51 GMT",
						"X-Request-Id": "req-recorded",
					},
					Body: realWorldRecordedBody,
				},
				Noise: realWorldNoise(),
			}
			// Date and X-Request-Id drift on every replay and are both noise;
			// Content-Type is not, and is identical here.
			actual := &models.HTTPResp{
				StatusCode: 200,
				Header: map[string]string{
					"Content-Type": "application/json; charset=utf-8",
					"Date":         "Thu, 06 Aug 2026 09:00:00 GMT",
					"X-Request-Id": "req-replayed",
				},
				Body: c.actual,
			}
			pass, _ := Match(tc, actual, map[string]map[string][]string{}, false, false, zap.NewNop(), false)
			if pass != c.wantPass {
				t.Errorf("pass = %v, want %v", pass, c.wantPass)
			}
		})
	}
}

// TestMatch_SectionedBodyNoise_ShortPathDoesNotLeak guards against a tempting
// but wrong "fix" for indexed noise paths: dropping the numeric segment from
// "0.id" yields the bare segment "id", and because noise keys are matched with
// strings.Contains that would silence every field whose name merely contains
// "id" — "width", "video" and "paid" all do.
func TestMatch_SectionedBodyNoise_ShortPathDoesNotLeak(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-short-stripped-path",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"width":10,"video":"a","paid":true}`,
		},
		Noise: map[string][]string{"body": {"0.id"}},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"width":99,"video":"b","paid":false}`,
	}

	pass, _ := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, false)

	assert.False(t, pass, "width/video/paid must not be silenced by a noise path that strips to \"id\"")
}

// TestMatch_GlobalWildcardBody_SurvivesSectionedNoise pins that the global
// "ignore every body" config keeps working on a test case that also carries a
// sectioned body-noise key — that key is no longer the skip sentinel itself.
func TestMatch_GlobalWildcardBody_SurvivesSectionedNoise(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-wildcard-with-sectioned-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"id":1,"name":"keploy","stock":99}`,
		},
		Noise: map[string][]string{"body": {"stock"}},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Body:       `{"id":2,"name":"totally-different","stock":100}`,
	}
	noiseConfig := map[string]map[string][]string{"body": {"*": {"*"}}}

	pass, _ := Match(tc, actualResponse, noiseConfig, false, false, logger, false)

	assert.True(t, pass, "the global body wildcard still means ignore the whole body")
}

// TestMatch_DoesNotMutateSharedNoiseConfig pins that a test case's own noise
// does not leak into the caller's global config and soften every later case.
func TestMatch_DoesNotMutateSharedNoiseConfig(t *testing.T) {
	logger := zap.NewNop()
	shared := map[string]map[string][]string{
		"body":   {},
		"header": {},
	}

	// Both key shapes are merged in, so both must be copied. The dotted shape is
	// the one that leaked before this change; the sectioned shape leaks only now
	// that it produces entries at all.
	noisy := &models.TestCase{
		Name:     "case-with-noise",
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"total":1,"stock":1}`},
		Noise:    map[string][]string{"body": {"total"}, "body.stock": {}},
	}
	Match(noisy, &models.HTTPResp{StatusCode: 200, Body: `{"total":2,"stock":2}`}, shared, false, false, logger, false)

	assert.Empty(t, shared["body"], "the shared global noise config must be left untouched")

	// A later test case that declares no noise must still catch both differences.
	clean := &models.TestCase{
		Name:     "case-without-noise",
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"total":1,"stock":1}`},
	}
	pass, _ := Match(clean, &models.HTTPResp{StatusCode: 200, Body: `{"total":2,"stock":2}`}, shared, false, false, logger, false)

	assert.False(t, pass, "noise from an earlier test case must not carry over")
}

// TestMatch_SectionedHeaderNoise_ScopesToTheListedHeader asserts on the
// per-header verdicts rather than only the overall pass flag, so the test
// cannot be satisfied by the body assertion failing instead.
func TestMatch_SectionedHeaderNoise_ScopesToTheListedHeader(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-sectioned-header-scope",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"X-Request-Id": "abc", "Content-Type": "application/json"},
			Body:       `{"ok":true}`,
		},
		Noise: map[string][]string{"header": {"X-Request-Id"}},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"X-Request-Id": "zzz", "Content-Type": "text/plain"},
		Body:       `{"ok":true}`,
	}

	pass, result := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, false)

	assert.False(t, pass)
	require.NotNil(t, result)
	verdicts := map[string]bool{}
	for _, h := range result.HeadersResult {
		verdicts[strings.ToLower(h.Expected.Key)] = h.Normal
	}
	normal, ok := verdicts["content-type"]
	require.True(t, ok, "Content-Type should be reported, got %v", verdicts)
	assert.False(t, normal, "Content-Type is not noise and differs")
	normal, ok = verdicts["x-request-id"]
	require.True(t, ok, "X-Request-Id should be reported, got %v", verdicts)
	assert.True(t, normal, "X-Request-Id is listed as noise")
}

// TestAssertionMatch_StatusCodeClass_ActualNon2xxPasses_3843 ensures that a status_code_class
// assertion of "5xx" passes when the actual response status code is 500. This guards against
// a regression where actualClass was computed from a hardcoded 200 instead of the real
// response status code, causing status_code_class to always evaluate against "2xx".
func TestAssertionMatch_StatusCodeClass_ActualNon2xxPasses_3843(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-status-code-class-5xx-pass",
		Assertions: map[models.AssertionType]interface{}{
			models.StatusCodeClass: "5xx",
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 500,
	}

	// Act
	pass, result := AssertionMatch(tc, actualResponse, logger)

	// Assert
	assert.True(t, pass, "Should pass because the actual status code 500 is in the 5xx class")
	require.NotNil(t, result)
}

// TestAssertionMatch_StatusCodeClass_ActualNon2xxFailsAgainst2xx_3843 ensures that a
// status_code_class assertion of "2xx" fails when the actual response status code is 500.
// Prior to the fix, actualClass was always "2xx" regardless of the actual response, so this
// assertion would have incorrectly passed.
func TestAssertionMatch_StatusCodeClass_ActualNon2xxFailsAgainst2xx_3843(t *testing.T) {
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-status-code-class-2xx-fail",
		Assertions: map[models.AssertionType]interface{}{
			models.StatusCodeClass: "2xx",
		},
	}
	actualResponse := &models.HTTPResp{
		StatusCode: 500,
	}

	// Act
	pass, result := AssertionMatch(tc, actualResponse, logger)

	// Assert
	assert.False(t, pass, "Should fail because the actual status code 500 is not in the 2xx class")
	require.NotNil(t, result)
}

package http

import (
	"strings"
	"testing"

	"github.com/k0kubun/pp/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// TestMatch_ABodyDifferenceFailsWithItsResult_987: the failure branch only
// explains a verdict, so a body difference fails with the result that says
// what differs, never with a nil result the report cannot show.
func TestMatch_ABodyDifferenceFailsWithItsResult_987(t *testing.T) {
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
	pass, res := Match(tc, actualResponse, map[string]map[string][]string{}, false, false, logger, true)
	assert.False(t, pass)
	require.NotNil(t, res)
	assert.False(t, res.BodyResult[0].Normal)
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

// THE ASSERTIONS PREDICATE is the one Match judges by and the one keploy test
// and keploy templatize refuse an untied array use by: a test case whose
// assertions hold anything but its noise is judged by AssertionMatch alone,
// and unless json_equal is among them no whole body is read. A noise
// assertion alone, an empty one included, judges nothing: the comparison
// does.
func TestAssertionsUntieBody(t *testing.T) {
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		replace    bool
		untie      string
	}{
		{"none", nil, false, ""},
		{"a noise assertion alone", map[models.AssertionType]interface{}{models.NoiseAssertion: map[string]interface{}{"body.id": []string{}}}, false, ""},
		{"an empty noise assertion alone", map[models.AssertionType]interface{}{models.NoiseAssertion: nil}, false, ""},
		{"status_code", map[models.AssertionType]interface{}{models.StatusCode: 200}, true, "status_code"},
		{"status_code beside noise", map[models.AssertionType]interface{}{models.StatusCode: 200, models.NoiseAssertion: map[string]interface{}{}}, true, "status_code"},
		{"header_exists and json_contains", map[models.AssertionType]interface{}{models.JsonContains: map[string]interface{}{}, models.HeaderExists: []interface{}{"X"}}, true, "header_exists, json_contains"},
		{"json_equal", map[models.AssertionType]interface{}{models.JsonEqual: true}, true, ""},
		{"json_equal beside status_code", map[models.AssertionType]interface{}{models.JsonEqual: true, models.StatusCode: 200}, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Assertions: c.assertions}
			assert.Equal(t, c.replace, AssertionsReplaceComparison(tc))
			assert.Equal(t, c.untie, AssertionsUntieBody(tc))
		})
	}

	// Match judges by the same predicate: a status_code assertion passes an
	// answer whose body differs, and without it the body is compared.
	tc := &models.TestCase{Name: "t", HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{"id":"a"}`}}
	answer := &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{"id":"b"}`}
	pass, _ := Match(tc, answer, nil, true, false, zap.NewNop(), false)
	assert.False(t, pass, "the comparison reads the body")
	tc.Assertions = map[models.AssertionType]interface{}{models.StatusCode: 200}
	pass, _ = Match(tc, answer, nil, true, false, zap.NewNop(), false)
	assert.True(t, pass, "the assertions judge alone")
	// An empty noise assertion (a YAML "noise:" left with no entries) judged
	// alone passed any answer, a status 0 "no answer" one included.
	tc.Assertions = map[models.AssertionType]interface{}{models.NoiseAssertion: nil}
	pass, _ = Match(tc, answer, nil, true, false, zap.NewNop(), false)
	assert.False(t, pass, "an empty noise assertion judged the answer")
	pass, _ = Match(tc, &models.HTTPResp{StatusCode: 0, Body: "connection refused"}, nil, true, false, zap.NewNop(), false)
	assert.False(t, pass, "an empty noise assertion passed no answer")
}

// A caller that failed the test case on its own (WithoutPassLine) gets no
// pass line from the assertions that pass its answer either, and still the
// failure line from those that fail it.
func TestMatchAssertionsHonourWithoutPassLine(t *testing.T) {
	var out strings.Builder
	saved := ppNew234
	ppNew234 = func() *pp.PrettyPrinter {
		p := pp.New()
		p.SetOutput(&out)
		return p
	}
	t.Cleanup(func() { ppNew234 = saved })
	for _, asserted := range []int{500, 201} {
		out.Reset()
		tc := &models.TestCase{
			Name:       "test-1",
			HTTPResp:   models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`},
			Assertions: map[models.AssertionType]interface{}{models.StatusCode: asserted},
		}
		pass, _ := Match(tc, &models.HTTPResp{StatusCode: 500, Body: `{"ok":true}`}, nil, false, false, zap.NewNop(), true, WithoutPassLine())
		require.Equal(t, asserted == 500, pass, "the verdict is the assertions'")
		require.NotContains(t, out.String(), "Testrun passed", "asserted %d", asserted)
		if pass {
			require.Empty(t, out.String(), "asserted %d", asserted)
		} else {
			require.Contains(t, out.String(), "Testrun failed", "asserted %d", asserted)
		}
	}
}

// When a test case's assertions judge its answer in place of the comparison
// (AssertionsReplaceComparison), Match's console says what they say. A
// comparison that matches printed "Testrun passed" for a test its failed
// assertion fails, and one that differs printed "Testrun failed", with its
// diff, for a test its assertions pass. What a failed assertion says is
// printed under the line, as a diff is. Compared quietly, it prints nothing,
// an assertion's failure included.
func TestMatchConsoleSaysWhatTheAssertionsThatJudgeItSay(t *testing.T) {
	var out strings.Builder
	saved := ppNew234
	ppNew234 = func() *pp.PrettyPrinter {
		p := pp.New()
		p.SetOutput(&out)
		return p
	}
	t.Cleanup(func() { ppNew234 = saved })
	for _, c := range []struct {
		name     string
		status   int
		asserted int
		wantPass bool
		want     string
	}{
		{"the answer matches, its assertion fails", 200, 201, false, "Testrun failed"},
		{"the answer differs, its assertion passes", 500, 500, true, "Testrun passed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{
				Name:       "test-1",
				HTTPResp:   models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`},
				Assertions: map[models.AssertionType]interface{}{models.StatusCode: c.asserted},
			}
			actual := func() *models.HTTPResp { return &models.HTTPResp{StatusCode: c.status, Body: `{"ok":true}`} }
			for _, loud := range []bool{true, false} {
				out.Reset()
				core, logs := observer.New(zap.ErrorLevel)
				// The log lines go where the lines go, so their order shows.
				console := zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(&out), zap.ErrorLevel)
				pass, _ := Match(tc, actual(), nil, false, false, zap.New(zapcore.NewTee(core, console)), loud)
				require.Equal(t, c.wantPass, pass, "the verdict is the assertions'")
				printed := out.String()
				if !loud {
					require.Empty(t, printed, "a quiet comparison printed")
					require.Zero(t, logs.Len(), "a quiet comparison logged %v", logs.All())
					continue
				}
				require.Contains(t, printed, c.want)
				require.NotContains(t, printed, map[bool]string{true: "Testrun failed", false: "Testrun passed"}[c.wantPass])
				require.Equal(t, !c.wantPass, logs.FilterMessage("status_code assertion failed").Len() == 1, "the failed assertion is said once, when it fails")
				if !c.wantPass {
					require.Less(t, strings.Index(printed, "Testrun failed"), strings.Index(printed, "status_code assertion failed"), "what the failed assertion says is under its line")
				}
			}
		})
	}
}

// An assertion keploy cannot judge fails its test case: one whose name this
// keploy does not implement, the keploy/tests (test-suite) types among them,
// and one whose value it cannot read. Each passed any answer, a status 0 "no
// answer" one included: the name fell to a Debug line, and a value of another
// shape, or a json_contains that is not JSON, compared against nothing.
func TestAssertionMatchFailsWhatItCannotJudge(t *testing.T) {
	noAnswer := &models.HTTPResp{StatusCode: 0, Body: "connection refused"}
	answer := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"id":"abc"}`}
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		actual     *models.HTTPResp
		said       string
	}{
		{"json_path, a keploy/tests type", map[models.AssertionType]interface{}{"json_path": "$.id"}, noAnswer, "keploy/tests"},
		{"custom_functions, a keploy/tests type", map[models.AssertionType]interface{}{"CUSTOM_FUNCTIONS": "function(){return true}"}, answer, "keploy/tests"},
		{"a name keploy does not know", map[models.AssertionType]interface{}{"STATUS_CODE": 200}, answer, "status_code"},
		{"header_equal that is not a map", map[models.AssertionType]interface{}{models.HeaderEqual: []interface{}{"Content-Type"}}, answer, "header_equal"},
		{"header_contains that is not a map", map[models.AssertionType]interface{}{models.HeaderContains: "json"}, answer, "header_contains"},
		{"header_matches that is not a map", map[models.AssertionType]interface{}{models.HeaderMatches: "application/.*"}, answer, "header_matches"},
		{"json_contains that is not JSON", map[models.AssertionType]interface{}{models.JsonContains: "id: abc"}, answer, "json_contains"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "test-1", HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"id":"abc"}`}, Assertions: c.assertions}
			core, logs := observer.New(zap.ErrorLevel)
			pass, _ := AssertionMatch(tc, c.actual, zap.New(core))
			require.False(t, pass, "an assertion keploy cannot judge passed the answer")
			var said []string
			for _, e := range logs.All() {
				said = append(said, e.Message)
				for _, f := range e.Context {
					said = append(said, f.String)
				}
			}
			require.Contains(t, strings.Join(said, " | "), c.said)
		})
	}
}

// A test case whose recording kept no body (over 1 MB, BodySkipped) is judged
// by its assertions on the answer's body as it came: json_contains reads it,
// and json_equal, which has no recorded body to compare it with, fails. Match
// cleared the answer's body first, so json_contains failed every answer and
// json_equal passed every one (empty against empty). As for any test case its
// assertions judge, they replace the whole comparison, the body-size check
// included; the result keeps no body the recording did not.
func TestMatchJudgesASkippedBodyByItsAssertions(t *testing.T) {
	recorded := models.HTTPResp{StatusCode: 200, BodySkipped: true, BodySize: 2 << 20}
	body := `{"id":"abc","pad":"` + strings.Repeat("x", 64) + `"}`
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		answer     string
		want       bool
	}{
		{"json_contains reads the answer's body", map[models.AssertionType]interface{}{models.JsonContains: map[string]interface{}{"id": "abc"}}, body, true},
		{"json_contains fails a body without it", map[models.AssertionType]interface{}{models.JsonContains: map[string]interface{}{"id": "xyz"}}, body, false},
		{"json_equal has no recorded body", map[models.AssertionType]interface{}{models.JsonEqual: true}, body, false},
		// The recorded body is empty too: compared as written, an empty
		// answer would pass.
		{"json_equal has no recorded body, for an empty answer too", map[models.AssertionType]interface{}{models.JsonEqual: true}, "", false},
		{"status_code judges alone, whatever the body's size", map[models.AssertionType]interface{}{models.StatusCode: 200}, body, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "test-1", HTTPResp: recorded, Assertions: c.assertions}
			pass, res := Match(tc, &models.HTTPResp{StatusCode: 200, Body: c.answer}, nil, false, false, zap.NewNop(), false)
			require.Equal(t, c.want, pass)
			require.Empty(t, res.BodyResult[0].Actual, "the result kept the body the recording skipped")
		})
	}
}

// When the assertions fail a test case whose recording kept no body, what
// Match says of them is what they found in the body as it came: an assertion
// that body satisfies is not said to fail.
func TestMatchSaysWhatTheAssertionsFoundInASkippedBody(t *testing.T) {
	saved := ppNew234
	ppNew234 = func() *pp.PrettyPrinter {
		p := pp.New()
		p.SetOutput(&strings.Builder{})
		return p
	}
	t.Cleanup(func() { ppNew234 = saved })
	tc := &models.TestCase{Name: "test-1", HTTPResp: models.HTTPResp{StatusCode: 200, BodySkipped: true, BodySize: 2 << 20}, Assertions: map[models.AssertionType]interface{}{
		models.StatusCode:   201,
		models.JsonContains: map[string]interface{}{"id": "abc"},
	}}
	core, logs := observer.New(zap.ErrorLevel)
	pass, _ := Match(tc, &models.HTTPResp{StatusCode: 200, Body: `{"id":"abc"}`}, nil, false, false, zap.New(core), true)
	require.False(t, pass)
	require.Equal(t, 1, logs.FilterMessageSnippet("status_code assertion failed").Len(), "%v", logs.All())
	require.Zero(t, logs.FilterMessageSnippet("json_contains").Len(), "json_contains, which the body as it came satisfies, was said to fail: %v", logs.All())
}

// A recording that kept no body says "comparing body size" only when Match
// emits its logs and the size is compared: never when compared quietly, nor
// when assertions judge the answer in its place.
func TestMatchSaysItComparesABodySizeOnlyWhenItDoes(t *testing.T) {
	saved := ppNew234
	ppNew234 = func() *pp.PrettyPrinter {
		p := pp.New()
		p.SetOutput(&strings.Builder{})
		return p
	}
	t.Cleanup(func() { ppNew234 = saved })
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		loud, said bool
	}{
		{"compared, with its logs", nil, true, true},
		{"compared quietly", nil, false, false},
		{"judged by its assertions", map[models.AssertionType]interface{}{models.StatusCode: 200}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "test-1", HTTPResp: models.HTTPResp{StatusCode: 200, BodySkipped: true, BodySize: 2 << 20}, Assertions: c.assertions}
			core, logs := observer.New(zap.InfoLevel)
			Match(tc, &models.HTTPResp{StatusCode: 200, Body: `{"id":"abc"}`}, nil, false, false, zap.New(core), c.loud)
			require.Equal(t, c.said, logs.FilterMessageSnippet("comparing body size").Len() == 1, "%v", logs.All())
		})
	}
}

// header_equal says a header failed only when it did.
func TestHeaderEqualLogsOnlyAFailure(t *testing.T) {
	tc := &models.TestCase{Name: "test-1", HTTPResp: models.HTTPResp{StatusCode: 200}, Assertions: map[models.AssertionType]interface{}{
		models.HeaderEqual: map[string]interface{}{"Content-Type": "application/json"},
	}}
	core, logs := observer.New(zap.DebugLevel)
	pass, _ := AssertionMatch(tc, &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}}, zap.New(core))
	require.True(t, pass)
	require.Zero(t, logs.FilterMessageSnippet("assertion failed").Len(), "a header that matched was logged as failed: %v", logs.All())
}

// The header assertions read each map shape a test case's file decodes to,
// now that a shape they do not read fails the test case: YAML keys a map by
// the assertions' key type (map[models.AssertionType]interface{}), or, when
// its keys are not all strings, map[interface{}]interface{}; JSON gives
// map[string]interface{}; a Go caller may hand map[string]string. And the
// noise beside them, as keploy record writes it, is noise: it fails nothing.
func TestAssertionMatchReadsEveryDecodedShape(t *testing.T) {
	right := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"id":"abc"}`}
	wrong := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "text/plain"}, Body: `{"id":"abc"}`}
	shapes := []struct {
		name string
		mk   func(k, v string) interface{}
	}{
		{"YAML", func(k, v string) interface{} { return map[models.AssertionType]interface{}{models.AssertionType(k): v} }},
		{"YAML, keys not all strings", func(k, v string) interface{} { return map[interface{}]interface{}{k: v} }},
		{"JSON", func(k, v string) interface{} { return map[string]interface{}{k: v} }},
		{"Go", func(k, v string) interface{} { return map[string]string{k: v} }},
	}
	for _, shape := range shapes {
		for _, a := range []struct {
			name models.AssertionType
			want string
		}{
			{models.HeaderEqual, "application/json"},
			{models.HeaderContains, "json"},
			{models.HeaderMatches, "^application/.*$"},
		} {
			t.Run(shape.name+"/"+string(a.name), func(t *testing.T) {
				tc := &models.TestCase{Name: "test-1", HTTPResp: models.HTTPResp{StatusCode: 200}, Assertions: map[models.AssertionType]interface{}{
					a.name:                shape.mk("Content-Type", a.want),
					models.NoiseAssertion: map[models.AssertionType]interface{}{"header.Date": []interface{}{}},
				}}
				pass, _ := Match(tc, right, nil, false, false, zap.NewNop(), false)
				require.True(t, pass, "it failed the right answer")
				pass, _ = Match(tc, wrong, nil, false, false, zap.NewNop(), false)
				require.False(t, pass, "it passed the wrong answer")
			})
		}
	}
}

// json_contains reads the object it names in each shape a test case's file
// decodes to: YAML keys a map, nested ones included, by the assertions' key
// type, and gives a whole number as an int, where the answer's body, read as
// JSON, holds a float64. Read as map[string]interface{} only, the YAML form
// failed every answer.
func TestJsonContainsReadsEveryDecodedShape(t *testing.T) {
	answer := &models.HTTPResp{StatusCode: 200, Body: `{"id":"abc","n":{"k":1},"tags":["a"]}`}
	for _, c := range []struct {
		name  string
		value interface{}
		want  bool
	}{
		{"YAML", map[models.AssertionType]interface{}{"id": "abc", "n": map[models.AssertionType]interface{}{"k": 1}}, true},
		{"YAML, a value the body does not hold", map[models.AssertionType]interface{}{"n": map[models.AssertionType]interface{}{"k": 2}}, false},
		{"YAML, keys not all strings", map[interface{}]interface{}{"tags": []interface{}{"a"}}, true},
		{"JSON", map[string]interface{}{"id": "abc", "n": map[string]interface{}{"k": float64(1)}}, true},
		{"a JSON string", `{"n":{"k":1}}`, true},
		{"a JSON string the body does not hold", `{"id":"xyz"}`, false},
		{"a list", []interface{}{"id"}, false},
		{"a number", 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "test-1", Assertions: map[models.AssertionType]interface{}{models.JsonContains: c.value}}
			pass, _ := AssertionMatch(tc, answer, zap.NewNop())
			require.Equal(t, c.want, pass)
		})
	}
}

// status_code_in reads one code as well as a list, as YAML (int) and JSON
// (float64) decode it, and a value with no code in it says it cannot be read.
func TestStatusCodeInReadsEveryShape(t *testing.T) {
	for _, c := range []struct {
		name  string
		value interface{}
		want  bool
		said  string
	}{
		{"a list", []interface{}{200, 201}, true, ""},
		{"a string", "200, 201", true, ""},
		{"one code, from YAML", 200, true, ""},
		{"one code, from JSON", float64(200), true, ""},
		{"another code", 404, false, "status_code_in assertion failed"},
		{"an empty list", []interface{}{}, false, "cannot be read"},
		{"no code", "ok", false, "cannot be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "test-1", Assertions: map[models.AssertionType]interface{}{models.StatusCodeIn: c.value}}
			core, logs := observer.New(zap.ErrorLevel)
			pass, _ := AssertionMatch(tc, &models.HTTPResp{StatusCode: 200}, zap.New(core))
			require.Equal(t, c.want, pass)
			if c.said != "" {
				require.Equal(t, 1, logs.FilterMessageSnippet(c.said).Len(), "%v", logs.All())
			}
		})
	}
}

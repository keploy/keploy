package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestUserVerification_SchemaMatch runs 5 distinct scenarios
// to demonstrate the Schema Match feature.
func TestUserVerification_SchemaMatch(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	tests := []struct {
		name        string
		description string
		expected    string
		actual      string
		shouldPass  bool
	}{
		{
			name:        "1. Superset Match (Success)",
			description: "Actual JSON has an EXTRA field 'timestamp' not in Expected. Strict match would FAIL, but Schema Match allows it.",
			expected:    `{"id": 1, "name": "Keploy"}`,
			actual:      `{"id": 1, "name": "Keploy", "timestamp": 123456}`,
			shouldPass:  true,
		},
		{
			name:        "2. Null Value Match (Success)",
			description: "Both Expected and Actual have 'val: null'. This should PASS.",
			expected:    `{"val": null}`,
			actual:      `{"val": null}`,
			shouldPass:  true,
		},
		{
			name:        "3. Missing Field (Failure)",
			description: "Actual JSON is MISSING the 'name' field required by Expected. This MUST fail.",
			expected:    `{"id": 1, "name": "Keploy"}`,
			actual:      `{"id": 1}`,
			shouldPass:  false,
		},
		{
			name:        "4. Type Mismatch (Failure)",
			description: "Expected 'id' is a NUMBER (1), but Actual is a STRING ('1'). Schema Match enforces types, so this FAILS.",
			expected:    `{"id": 1}`,
			actual:      `{"id": "1"}`,
			shouldPass:  false,
		},
		{
			name:        "5. Array Length Mismatch (Success - Relaxed)",
			description: "Expected list has 2 items, Actual has only 1. Since length check is relaxed, this PASSES.",
			expected:    `{"tags": ["a", "b"]}`,
			actual:      `{"tags": ["a"]}`,
			shouldPass:  true,
		},
	}

	t.Log("\n--- Running 5 User Verification Test Cases for Schema Match Feature ---\n")

	for _, tc := range tests {
		t.Logf("\n🔹 TEST CASE: %s", tc.name)
		t.Logf("   Description: %s", tc.description)

		// Create mock models
		// Using compact JSON and unmarshal to map to simulate real flow if needed,
		// but simple string assignments are enough for unit test setup here as MatchSchema handles unmarshalling internally if body is string?
		// Checking schema_match.go: It unmarshals into map[string]interface{}

		tCase := &models.TestCase{
			HTTPResp: models.HTTPResp{
				Body:   tc.expected,
				Header: map[string]string{"Content-Type": "application/json"},
			},
		}
		actualResp := &models.HTTPResp{
			Body:   tc.actual,
			Header: map[string]string{"Content-Type": "application/json"},
		}

		// Run Logic
		pass, _ := MatchSchema(tCase, actualResp, logger)

		// Verify Result
		if pass == tc.shouldPass {
			t.Logf("✅ RESULT: As Expected (Pass: %v)", pass)
		} else {
			t.Errorf("❌ RESULT: Unexpected! Expected Pass: %v, Got: %v", tc.shouldPass, pass)
		}
	}
	t.Log("\n--- Verification Complete ---\n")
}

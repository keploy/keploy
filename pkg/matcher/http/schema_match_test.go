package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestSchemaMatch(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	tests := []struct {
		name     string
		expected string
		actual   string
		want     bool
	}{
		{
			name:     "Exact Match",
			expected: `{"key": "value", "id": 1}`,
			actual:   `{"key": "value", "id": 1}`,
			want:     true,
		},
		{
			name:     "Superset (Match)",
			expected: `{"key": "value"}`,
			actual:   `{"key": "value", "extra": "field"}`,
			want:     true,
		},
		{
			name:     "Missing Key (Fail)",
			expected: `{"key": "value", "id": 1}`,
			actual:   `{"key": "value"}`,
			want:     false,
		},
		{
			name:     "Type Mismatch (Fail)",
			expected: `{"id": 1}`,
			actual:   `{"id": "1"}`,
			want:     false,
		},
		{
			name:     "Value Mismatch Same Type (Match)",
			expected: `{"key": "value1"}`,
			actual:   `{"key": "value2"}`,
			want:     true,
		},
		{
			name:     "Nested Object Match",
			expected: `{"user": {"id": 1, "name": "test"}}`,
			actual:   `{"user": {"id": 2, "name": "changed", "age": 30}}`,
			want:     true,
		},
		{
			name:     "Nested Object Missing Key (Fail)",
			expected: `{"user": {"id": 1, "name": "test"}}`,
			actual:   `{"user": {"id": 2}}`,
			want:     false,
		},
		{
			name:     "Array Exact Match",
			expected: `{"list": [1, 2, 3]}`,
			actual:   `{"list": [1, 2, 3]}`,
			want:     true,
		},
		{
			name:     "Array Length Mismatch (Pass - Relaxed)",
			expected: `{"list": [1, 2]}`,
			actual:   `{"list": [1]}`,
			want:     true,
		},
		{
			name:     "Array Superset (Match)",
			expected: `{"list": [1, 2]}`,
			actual:   `{"list": [1, 2, 3]}`,
			want:     true,
		},
		{
			name:     "Array Item Type Mismatch (Fail)",
			expected: `{"list": [1, 2]}`,
			actual:   `{"list": [1, "2"]}`,
			want:     false,
		},
		{
			name:     "Complex Nested Structure",
			expected: `{"data": [{"id": 1, "attrs": {"enabled": true}}]}`,
			actual:   `{"data": [{"id": 99, "attrs": {"enabled": false, "new": 1}, "extra": 0}]}`,
			want:     true,
		},
		{
			name:     "Null Value Match",
			expected: `{"val": null}`,
			actual:   `{"val": null}`,
			want:     true,
		},
		{
			name:     "Null Type Mismatch (Fail)",
			expected: `{"val": 1}`,
			actual:   `{"val": null}`,
			want:     false,
		},
		{
			name:     "Null Expected Mismatch (Fail)",
			expected: `{"val": null}`,
			actual:   `{"val": 1}`,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &models.TestCase{
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
					Body:       tt.expected,
				},
			}
			actualResp := &models.HTTPResp{
				StatusCode: 200,
				Body:       tt.actual,
			}

			got, result := MatchSchema(tc, actualResp, logger)
			if got != tt.want {
				t.Errorf("MatchSchema() = %v, want %v. Result: %+v", got, tt.want, result)
			}
		})
	}
}

func TestSchemaMatch_Headers(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tests := []struct {
		name     string
		expected map[string]string
		actual   map[string]string
		want     bool
	}{
		{
			name:     "Header Exact Match",
			expected: map[string]string{"Content-Type": "application/json"},
			actual:   map[string]string{"Content-Type": "application/json"},
			want:     true,
		},
		{
			name:     "Header Superset",
			expected: map[string]string{"Content-Type": "application/json"},
			actual:   map[string]string{"Content-Type": "application/json", "X-Trace-Id": "123"},
			want:     true,
		},
		{
			name:     "Header Missing",
			expected: map[string]string{"Content-Type": "application/json", "X-Required": "true"},
			actual:   map[string]string{"Content-Type": "application/json"},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &models.TestCase{
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
					Header:     tt.expected,
					Body:       `{}`,
				},
			}
			actualResp := &models.HTTPResp{
				StatusCode: 200,
				Header:     tt.actual,
				Body:       `{}`,
			}

			got, result := MatchSchema(tc, actualResp, logger)
			if got != tt.want {
				t.Errorf("MatchSchema() = %v, want %v. Result: %+v", got, tt.want, result)
			}
		})
	}
}

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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

			pass, result := MatchSchema(tCase, actualResp, logger)
			if pass != tc.shouldPass {
				t.Errorf("MatchSchema() = %v, want %v. Result: %+v", pass, tc.shouldPass, result)
			}
		})
	}
}

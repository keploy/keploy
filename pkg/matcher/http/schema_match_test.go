package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestSchemaMatch_HeadersResult verifies that result.HeadersResult is populated
// and the pass/fail banner matches the actual outcome (regression for issue #4221).
func TestSchemaMatch_HeadersResult(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	tests := []struct {
		name                string
		recordedHeaders     map[string]string
		actualHeaders       map[string]string
		wantPass            bool
		wantHeadersLen      int
		wantMissingKey      string
	}{
		{
			name:            "all recorded headers present — pass, HeadersResult populated",
			recordedHeaders: map[string]string{"Content-Type": "application/json", "X-Service": "svc"},
			actualHeaders:   map[string]string{"Content-Type": "application/json", "X-Service": "svc"},
			wantPass:        true,
			wantHeadersLen:  2,
			wantMissingKey:  "",
		},
		{
			name:            "recorded header missing in actual — fail, HeadersResult populated",
			recordedHeaders: map[string]string{"Content-Type": "application/json", "X-Service": "schema-match-bug-repro"},
			actualHeaders:   map[string]string{"Content-Type": "application/json"},
			wantPass:        false,
			wantHeadersLen:  2,
			wantMissingKey:  "X-Service",
		},
		{
			name:            "actual has extra headers — pass, only recorded headers in HeadersResult",
			recordedHeaders: map[string]string{"Content-Type": "application/json"},
			actualHeaders:   map[string]string{"Content-Type": "application/json", "X-Extra": "bonus"},
			wantPass:        true,
			wantHeadersLen:  1,
			wantMissingKey:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &models.TestCase{
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
					Header:     tt.recordedHeaders,
					Body:       `{}`,
				},
			}
			actualResp := &models.HTTPResp{
				StatusCode: 200,
				Header:     tt.actualHeaders,
				Body:       `{}`,
			}

			got, result := MatchSchema(tc, actualResp, logger)

			if got != tt.wantPass {
				t.Errorf("pass = %v, want %v", got, tt.wantPass)
			}

			if len(result.HeadersResult) != tt.wantHeadersLen {
				t.Errorf("len(HeadersResult) = %d, want %d; got: %+v",
					len(result.HeadersResult), tt.wantHeadersLen, result.HeadersResult)
			}

			if tt.wantMissingKey != "" {
				found := false
				for _, hr := range result.HeadersResult {
					if hr.Expected.Key == tt.wantMissingKey && !hr.Normal {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected HeadersResult to contain Normal=false entry for key %q, got: %+v",
						tt.wantMissingKey, result.HeadersResult)
				}
			}
		})
	}
}

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

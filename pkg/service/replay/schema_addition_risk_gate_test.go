package replay

import (
	"fmt"
	"testing"

	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// jsonResp builds a 200 JSON response whose Content-Length matches the body, so
// an additive change moves the header exactly as it does in production.
func jsonResp(body string) models.HTTPResp {
	return models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type":   "application/json",
			"Content-Length": fmt.Sprint(len(body)),
		},
		Body: body,
	}
}

// TestQualifiesForHTTPResponseSchemaAdditionPassAgainstRealMatcher drives real
// request/response pairs through httpMatcher.Match, so the predicate is checked
// against results the matcher can actually emit rather than hand-built fixtures.
//
// This is the case a fixture-only suite cannot express. FailureInfo.Risk is the
// MAX across status/header/body, and pkg/matcher/http/match.go appends
// HeaderChanged only inside a block that unconditionally maxes in Medium — so
// {Risk: Low, Category: [SchemaAdded, HeaderChanged]} is unreachable, and a
// gate on FailureInfo.Risk silently disables the Content-Length branch
// entirely. The body-only grade lives on FailureInfo.Assessment.
func TestQualifiesForHTTPResponseSchemaAdditionPassAgainstRealMatcher(t *testing.T) {
	tests := []struct {
		name         string
		expected     string
		actual       string
		wantBodyRisk models.RiskLevel
		wantQualify  bool
		why          string
	}{
		{
			name:         "additive only, with the Content-Length it implies",
			expected:     `{"a":1}`,
			actual:       `{"a":1,"b":2}`,
			wantBodyRisk: models.Low,
			wantQualify:  true,
			why:          "the case this pass exists for: a field was added and nothing else moved",
		},
		{
			name:         "additive plus a changed value on an existing field",
			expected:     `{"a":1}`,
			actual:       `{"a":2,"b":2}`,
			wantBodyRisk: models.Medium,
			wantQualify:  false,
			why:          "#4578: a value change must not auto-pass by riding along with an added field",
		},
		{
			name:         "value change alone",
			expected:     `{"a":1}`,
			actual:       `{"a":2}`,
			wantBodyRisk: models.Medium,
			wantQualify:  false,
			why:          "no addition at all; nothing to treat as backward compatible",
		},
		{
			name:         "removed field",
			expected:     `{"a":1,"b":2}`,
			actual:       `{"a":1}`,
			wantBodyRisk: models.High,
			wantQualify:  false,
			why:          "a removal is a breaking change",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &models.TestCase{HTTPResp: jsonResp(tt.expected)}
			actual := jsonResp(tt.actual)

			pass, result := httpMatcher.Match(tc, &actual, map[string]map[string][]string{},
				true, false, zap.NewNop(), false)
			if pass {
				t.Fatalf("precondition: matcher should report a mismatch for %q vs %q", tt.expected, tt.actual)
			}
			if result.FailureInfo.Assessment == nil {
				t.Fatalf("matcher produced no body assessment; the gate has nothing to read")
			}
			if got := result.FailureInfo.Assessment.Risk; got != tt.wantBodyRisk {
				t.Fatalf("body risk = %q, want %q (categories %v)",
					got, tt.wantBodyRisk, result.FailureInfo.Category)
			}
			if got := qualifiesForHTTPResponseSchemaAdditionPass(result); got != tt.wantQualify {
				t.Fatalf("qualifies = %v, want %v: %s\n  aggregate risk %q, body risk %q, categories %v",
					got, tt.wantQualify, tt.why,
					result.FailureInfo.Risk, result.FailureInfo.Assessment.Risk, result.FailureInfo.Category)
			}
		})
	}
}

// TestQualifiesForHTTPResponseSchemaAdditionPassUnits covers shapes the
// matcher-driven table cannot reach directly — a nil assessment, and a header
// diff that is not Content-Length. Every fixture here keeps FailureInfo.Risk
// consistent with what the matcher would emit: Medium whenever HeaderChanged is
// present, since that category is only ever appended alongside a Medium floor.
func TestQualifiesForHTTPResponseSchemaAdditionPassUnits(t *testing.T) {
	contentLengthOnly := []models.HeaderResult{{
		Normal:   false,
		Expected: models.Header{Key: "Content-Length", Value: []string{"7"}},
		Actual:   models.Header{Key: "Content-Length", Value: []string{"13"}},
	}}

	tests := []struct {
		name   string
		result *models.Result
		want   bool
	}{
		{
			name:   "nil result",
			result: nil,
		},
		{
			name: "no body assessment to read",
			result: &models.Result{FailureInfo: models.FailureInfo{
				Risk:     models.Medium,
				Category: []models.FailureCategory{models.SchemaAdded, models.HeaderChanged},
			}},
		},
		{
			name: "low body risk, no header compared (chunked response)",
			result: &models.Result{FailureInfo: models.FailureInfo{
				Risk:       models.Low,
				Category:   []models.FailureCategory{models.SchemaAdded},
				Assessment: &models.FailureAssessment{Risk: models.Low},
			}},
			want: true,
		},
		{
			name: "low body risk, Content-Length moved with it",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:       models.Medium, // forced by HeaderChanged
					Category:   []models.FailureCategory{models.SchemaAdded, models.HeaderChanged},
					Assessment: &models.FailureAssessment{Risk: models.Low},
				},
				HeadersResult: contentLengthOnly,
			},
			want: true,
		},
		{
			name: "low body risk, but a header other than Content-Length moved",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:       models.Medium,
					Category:   []models.FailureCategory{models.SchemaAdded, models.HeaderChanged},
					Assessment: &models.FailureAssessment{Risk: models.Low},
				},
				HeadersResult: []models.HeaderResult{{
					Normal:   false,
					Expected: models.Header{Key: "X-Trace-Id", Value: []string{"a"}},
					Actual:   models.Header{Key: "X-Trace-Id", Value: []string{"b"}},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qualifiesForHTTPResponseSchemaAdditionPass(tt.result); got != tt.want {
				t.Fatalf("qualifies = %v, want %v", got, tt.want)
			}
		})
	}
}

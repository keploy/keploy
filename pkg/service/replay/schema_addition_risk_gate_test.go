package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// contentLengthHeaders is the header diff an additive body change always
// produces: the body grew, so Content-Length moved and nothing else did.
func contentLengthHeaders() []models.HeaderResult {
	return []models.HeaderResult{
		{
			Normal:   false,
			Expected: models.Header{Key: "Content-Length", Value: []string{"197"}},
			Actual:   models.Header{Key: "Content-Length", Value: []string{"223"}},
		},
	}
}

// TestQualifiesForHTTPResponseSchemaAdditionPassRiskGate pins the rule that the
// additive auto-pass applies to a Low-risk diff only.
//
// The regression it guards (#4578): AssessJSON grades "new fields only" as Low
// and "new fields plus value changes on existing fields" as Medium, and files
// BOTH under category SchemaAdded. The Content-Length branch used to skip the
// Risk check, so a Medium diff — a changed value riding along with an added
// field — auto-passed. Adding a field always changes Content-Length, so that
// branch matched nearly every additive diff and the Low gate never applied.
func TestQualifiesForHTTPResponseSchemaAdditionPassRiskGate(t *testing.T) {
	tests := []struct {
		name    string
		result  *models.Result
		want    bool
		rejects string
	}{
		{
			name:   "nil result",
			result: nil,
			want:   false,
		},
		{
			name: "low risk, only new fields",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:     models.Low,
					Category: []models.FailureCategory{models.SchemaAdded},
				},
			},
			want: true,
		},
		{
			name: "low risk, new fields plus the Content-Length they imply",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:     models.Low,
					Category: []models.FailureCategory{models.SchemaAdded, models.HeaderChanged},
				},
				HeadersResult: contentLengthHeaders(),
			},
			want: true,
		},
		{
			name: "medium risk, new fields plus a changed value",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:     models.Medium,
					Category: []models.FailureCategory{models.SchemaAdded},
				},
			},
			want:    false,
			rejects: "a value change must not auto-pass just because a field was added",
		},
		{
			name: "medium risk, new fields plus a changed value, with Content-Length",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:     models.Medium,
					Category: []models.FailureCategory{models.SchemaAdded, models.HeaderChanged},
				},
				HeadersResult: contentLengthHeaders(),
			},
			want:    false,
			rejects: "the Content-Length branch must not bypass the Risk gate",
		},
		{
			name: "high risk, removed or retyped fields",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:     models.High,
					Category: []models.FailureCategory{models.SchemaBroken},
				},
			},
			want: false,
		},
		{
			name: "low risk, but a header other than Content-Length moved",
			result: &models.Result{
				FailureInfo: models.FailureInfo{
					Risk:     models.Low,
					Category: []models.FailureCategory{models.SchemaAdded, models.HeaderChanged},
				},
				HeadersResult: []models.HeaderResult{
					{
						Normal:   false,
						Expected: models.Header{Key: "X-Trace-Id", Value: []string{"a"}},
						Actual:   models.Header{Key: "X-Trace-Id", Value: []string{"b"}},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := qualifiesForHTTPResponseSchemaAdditionPass(tt.result)
			if got != tt.want {
				msg := tt.rejects
				if msg == "" {
					msg = "unexpected qualification verdict"
				}
				t.Fatalf("qualifiesForHTTPResponseSchemaAdditionPass() = %v, want %v: %s", got, tt.want, msg)
			}
		})
	}
}

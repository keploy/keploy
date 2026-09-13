package replay

import (
	"strconv"
	"testing"

	"go.keploy.io/server/v3/config"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestCompareHTTPRespForReplaySchemaAddition drives the real matcher with the
// bodies from #4578, including the Content-Length change a grown body brings.
func TestCompareHTTPRespForReplaySchemaAddition(t *testing.T) {
	const expected = `{"apiVersion":"v2","configCount":2,"count":8,"products":[{"id":1,"name":"widget"}]}`

	cases := []struct {
		name     string
		actual   string
		wantPass bool
	}{
		{
			name:     "added field only passes",
			actual:   `{"apiVersion":"v2","configCount":2,"count":8,"featureConfig":null,"products":[{"id":1,"name":"widget"}]}`,
			wantPass: true,
		},
		{
			name:     "added field plus changed count fails",
			actual:   `{"apiVersion":"v2","configCount":2,"count":10,"featureConfig":null,"products":[{"id":1,"name":"widget"}]}`,
			wantPass: false,
		},
		{
			name:     "changed count alone fails",
			actual:   `{"apiVersion":"v2","configCount":2,"count":10,"products":[{"id":1,"name":"widget"}]}`,
			wantPass: false,
		},
	}

	for _, tc := range cases {
		for _, emitFailureLogs := range []bool{false, true} {
			t.Run(tc.name+"/emitFailureLogs="+strconv.FormatBool(emitFailureLogs), func(t *testing.T) {
				r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}

				testCase := &models.TestCase{
					Name:     "test-1",
					HTTPResp: jsonHTTPResp(expected),
				}
				actual := jsonHTTPResp(tc.actual)

				pass, result := r.compareHTTPRespForReplay(testCase, &actual, "test-set-0", emitFailureLogs)
				if pass != tc.wantPass {
					t.Fatalf("compareHTTPRespForReplay() pass = %v, want %v (failure info: %+v)", pass, tc.wantPass, result.FailureInfo)
				}
				if !tc.wantPass && len(result.BodyResult) > 0 && result.BodyResult[0].Normal {
					t.Errorf("body result marked normal for a failing case")
				}
			})
		}
	}
}

func jsonHTTPResp(body string) models.HTTPResp {
	return models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type":   "application/json",
			"Content-Length": strconv.Itoa(len(body)),
		},
		Body: body,
	}
}

// TestQualifiesForHTTPResponseSchemaAdditionPass guards the additive-schema
// auto-pass: it may only fire when the response gained fields and nothing that
// was already there changed (#4578).
func TestQualifiesForHTTPResponseSchemaAdditionPass(t *testing.T) {
	const expected = `{"apiVersion":"v2","configCount":2,"count":8,"products":[{"id":1}]}`

	contentLengthDiff := []models.HeaderResult{{
		Normal:   false,
		Expected: models.Header{Key: "Content-Length", Value: []string{"70"}},
		Actual:   models.Header{Key: "Content-Length", Value: []string{"92"}},
	}}

	cases := []struct {
		name       string
		actual     string
		categories []models.FailureCategory
		headers    []models.HeaderResult
		want       bool
	}{
		{
			name:       "added field only",
			actual:     `{"apiVersion":"v2","configCount":2,"count":8,"featureConfig":null,"products":[{"id":1}]}`,
			categories: []models.FailureCategory{models.SchemaAdded},
			want:       true,
		},
		{
			name:       "added field only with content-length diff",
			actual:     `{"apiVersion":"v2","configCount":2,"count":8,"featureConfig":null,"products":[{"id":1}]}`,
			categories: []models.FailureCategory{models.HeaderChanged, models.SchemaAdded},
			headers:    contentLengthDiff,
			want:       true,
		},
		{
			name:       "added field plus changed scalar",
			actual:     `{"apiVersion":"v2","configCount":2,"count":10,"featureConfig":null,"products":[{"id":1}]}`,
			categories: []models.FailureCategory{models.SchemaAdded},
			want:       false,
		},
		{
			// The case from #4578: the grown body also changes Content-Length,
			// which routed it through the branch that ignored risk.
			name:       "added field plus changed scalar with content-length diff",
			actual:     `{"apiVersion":"v2","configCount":2,"count":10,"featureConfig":null,"products":[{"id":1}]}`,
			categories: []models.FailureCategory{models.HeaderChanged, models.SchemaAdded},
			headers:    contentLengthDiff,
			want:       false,
		},
		{
			name:       "added field plus changed nested value with content-length diff",
			actual:     `{"apiVersion":"v2","configCount":2,"count":8,"featureConfig":null,"products":[{"id":2}]}`,
			categories: []models.FailureCategory{models.HeaderChanged, models.SchemaAdded},
			headers:    contentLengthDiff,
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assessment, err := matcherUtils.ComputeFailureAssessmentJSON(expected, tc.actual, nil, true)
			if err != nil || assessment == nil {
				t.Fatalf("ComputeFailureAssessmentJSON() = %v, %v", assessment, err)
			}

			result := &models.Result{
				HeadersResult: tc.headers,
				FailureInfo: models.FailureInfo{
					Risk:       assessment.Risk,
					Category:   tc.categories,
					Assessment: assessment,
				},
			}

			if got := qualifiesForHTTPResponseSchemaAdditionPass(result); got != tc.want {
				t.Errorf("qualifiesForHTTPResponseSchemaAdditionPass() = %v, want %v (assessment: %+v)", got, tc.want, assessment)
			}
		})
	}
}

func TestQualifiesForHTTPResponseSchemaAdditionPassWithoutAssessment(t *testing.T) {
	result := &models.Result{
		FailureInfo: models.FailureInfo{
			Risk:     models.Low,
			Category: []models.FailureCategory{models.SchemaAdded},
		},
	}

	if qualifiesForHTTPResponseSchemaAdditionPass(result) {
		t.Error("qualifiesForHTTPResponseSchemaAdditionPass() = true without a body assessment, want false")
	}
}

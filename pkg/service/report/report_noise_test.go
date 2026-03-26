package report

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
)

func TestRenderSingleFailedTest_WithNoise(t *testing.T) {
	testResult := models.TestResult{
		Name:       "test-1",
		TestCaseID: "test-case-1",
		Status:     models.TestStatusFailed,
		Noise: map[string][]string{
			"body.timestamp": {},
		},
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   true,
				Expected: 200,
				Actual:   200,
			},
			BodyResult: []models.BodyResult{
				{
					Normal:   false,
					Type:     models.JSON,
					Expected: `{"status": "ok", "timestamp": "2023-01-01T00:00:00Z"}`,
					Actual:   `{"status": "ok", "timestamp": "2023-01-02T00:00:00Z"}`,
				},
			},
		},
	}

	r := &Report{
		logger: newTestLogger(),
		config: &config.Config{},
	}
	ctx := context.Background()
	var sb strings.Builder
	err := r.renderSingleFailedTest(ctx, &sb, testResult)
	if err != nil {
		t.Fatalf("renderSingleFailedTest failed: %v", err)
	}

	output := sb.String()
	// If noise is working, the timestamp field is replaced by <NOISE> in both expected and actual.
    // So GenerateTableDiff sees identical content (or content with <NOISE> in both).
    // It should NOT report a diff for timestamp.
    
	if strings.Contains(output, "2023-01-01T00:00:00Z") || strings.Contains(output, "2023-01-02T00:00:00Z") {
		t.Error("output should not contain timestamp values as they are noisy")
        t.Logf("Output: %s", output)
	}
}

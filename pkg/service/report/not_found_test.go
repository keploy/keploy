package report

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// notFoundCase is the row of a test case the run selected by name that its
// test set does not hold: FAILED, never sent, with why.
func notFoundCase() models.TestResult {
	return models.TestResult{
		Name: "set-a", TestCaseID: "test-9", Status: models.TestStatusFailed,
		Result: models.Result{StatusCode: models.IntResult{Normal: true}, HeadersResult: []models.HeaderResult{}, BodyResult: []models.BodyResult{}},
		FailureInfo: models.FailureInfo{
			Risk: models.High, Category: []models.FailureCategory{models.TestCaseNotFound},
			NotFound: "test case test-9 not found in set-a",
		},
	}
}

// EVERY REPORT FORMAT says why a selected test case that is not on disk
// failed, and shows nothing it did not compare: keploy report, --full, JUnit
// and --format json.
func TestReports_SayASelectedTestCaseWasNotFound(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var compact, full strings.Builder
	if err := r.renderSingleFailedTest(context.Background(), &compact, notFoundCase()); err != nil {
		t.Fatal(err)
	}
	if err := r.renderSingleFullBodyFailedTest(context.Background(), &full, notFoundCase()); err != nil {
		t.Fatal(err)
	}
	f := buildFailure(notFoundCase())
	if f.Message != "selected test case not found in its test set, not run [HIGH-RISK]" {
		t.Errorf("JUnit message %q", f.Message)
	}
	for format, out := range map[string]string{"keploy report": compact.String(), "--full": full.String(), "junit": f.Text} {
		if !strings.Contains(out, "not run: test case test-9 not found in set-a") {
			t.Errorf("%s does not say why the test case was not run:\n%s", format, out)
		}
		for _, never := range []string{"status", "header", "No differences found", "found no difference", "CHANGES", "EXPECT", "ACTUAL"} {
			if strings.Contains(out, never) {
				t.Errorf("%s shows %q for a test case never run:\n%s", format, never, out)
			}
		}
	}
	for format, out := range map[string]string{"keploy report": compact.String(), "--full": full.String()} {
		if want := "Testrun failed for set-a/test-9 [HIGH-RISK] [TEST_CASE_NOT_FOUND]\n  not run: test case test-9 not found in set-a\n"; !strings.HasPrefix(out, want) {
			t.Errorf("%s does not start %q:\n%s", format, want, out)
		}
	}
	data, err := json.Marshal(buildTestVerdict("run", "set-a", notFoundCase(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"status":"FAILED","failure_categories":["TEST_CASE_NOT_FOUND"]`) {
		t.Errorf("--format json does not file the test case as not found: %s", data)
	}

	// A test case that was run and failed says no such thing.
	sent := templateOnlyFailure()
	var sentOut, sentFull strings.Builder
	if err := r.renderSingleFailedTest(context.Background(), &sentOut, sent); err != nil {
		t.Fatal(err)
	}
	if err := r.renderSingleFullBodyFailedTest(context.Background(), &sentFull, sent); err != nil {
		t.Fatal(err)
	}
	for format, out := range map[string]string{"keploy report": sentOut.String(), "--full": sentFull.String()} {
		if strings.Contains(out, "not run") || !strings.Contains(out, "TEMPLATE VARIABLES THE ANSWER DID NOT SATISFY") {
			t.Errorf("%s renders a test case that was run as not run:\n%s", format, out)
		}
	}
	if m := buildFailure(sent); m.Message != "test assertion failed [HIGH-RISK]" || strings.Contains(m.Text, "not run") {
		t.Errorf("JUnit renders a test case that was run as not run: %+v", m)
	}
}

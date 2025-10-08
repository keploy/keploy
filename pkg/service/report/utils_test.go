package report

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// --- Mocks ---
type mockReportDB struct {
	GetReportFunc      func(ctx context.Context, runID, testSetID string) (*models.TestReport, error)
	GetAllTestRunIDsFn func(ctx context.Context) ([]string, error)
}

func (m *mockReportDB) GetReport(ctx context.Context, runID, testSetID string) (*models.TestReport, error) {
	return m.GetReportFunc(ctx, runID, testSetID)
}
func (m *mockReportDB) GetAllTestRunIDs(ctx context.Context) ([]string, error) {
	return m.GetAllTestRunIDsFn(ctx)
}

type mockTestDB struct {
	GetReportTestSetsFn func(ctx context.Context, runID string) ([]string, error)
}

func (m *mockTestDB) GetReportTestSets(ctx context.Context, runID string) ([]string, error) {
	return m.GetReportTestSetsFn(ctx, runID)
}

// --- Helpers ---
func newTestLogger() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

// --- Tests ---
func TestPrintSummary(t *testing.T) {
	reports := map[string]*models.TestReport{
		"suite1": {
			Name:      "suite1",
			Total:     2,
			Success:   1,
			Failure:   1,
			TimeTaken: "1.5s",
			Tests: []models.TestResult{
				{TestCaseID: "1", Status: models.TestStatusPassed, TimeTaken: "1s"},
				{TestCaseID: "2", Status: models.TestStatusFailed, TimeTaken: "0.5s"},
			},
		},
	}
	r := &Report{
		logger: newTestLogger(),
		config: nil,
		out:    bufio.NewWriterSize(os.Stdout, 4096),
	}
	err := r.printSummary(reports)
	if err != nil {
		t.Fatalf("printSummary failed: %v", err)
	}
}

func TestPrintTests_Passed(t *testing.T) {
	tests := []models.TestResult{
		{TestCaseID: "1", Name: "Test1", Status: models.TestStatusPassed, TimeTaken: "1s"},
		{TestCaseID: "2", Name: "Test2", Status: models.TestStatusPassed, TimeTaken: "2s"},
	}
	r := &Report{
		logger: newTestLogger(),
		config: nil,
		out:    bufio.NewWriterSize(os.Stdout, 4096),
	}
	if err := r.printTests(tests); err != nil {
		t.Fatalf("printTests failed: %v", err)
	}
}

func TestFilterTestsByIDs(t *testing.T) {
	tests := []models.TestResult{
		{TestCaseID: "1"},
		{TestCaseID: "2"},
		{TestCaseID: "3"},
	}
	ids := []string{"2", "3"}
	r := &Report{}
	filtered := r.filterTestsByIDs(tests, ids)
	if len(filtered) != 2 {
		t.Fatalf("expected 2, got %d", len(filtered))
	}
	if filtered[0].TestCaseID != "2" || filtered[1].TestCaseID != "3" {
		t.Fatalf("unexpected filter result: %+v", filtered)
	}
}

func TestExtractFailedTestsFromResults(t *testing.T) {
	tests := []models.TestResult{
		{TestCaseID: "1", Status: models.TestStatusPassed},
		{TestCaseID: "2", Status: models.TestStatusFailed},
		{TestCaseID: "3", Status: models.TestStatusFailed},
	}
	r := &Report{}
	failed := r.extractFailedTestsFromResults(tests)
	if len(failed) != 2 {
		t.Fatalf("expected 2 failed, got %d", len(failed))
	}
}

func TestPrintDefaultBodyDiff(t *testing.T) {
	r := &Report{
		logger: newTestLogger(),
		config: nil,
	}
	body := models.BodyResult{
		Actual:   "{\"foo\":1}",
		Expected: "{\"foo\":2}",
	}
	if err := r.printDefaultBodyDiff(body); err != nil {
		t.Fatalf("printDefaultBodyDiff failed: %v", err)
	}
}

func TestApplyCliColorsToDiff(t *testing.T) {
	// diff := "Path: foo\n  Old: 1\n  New: 2"
	// colored := applyCliColorsToDiff(diff)
	// if !strings.Contains(colored, "\x1b[33m") || !strings.Contains(colored, "\x1b[31m") || !strings.Contains(colored, "\x1b[32m") {
	// 	t.Fatalf("color codes missing: %q", colored)
	// }

	diff := "Path: foo\n  Expected: 1\n  Actual: 2"

	colored := applyCliColorsToDiff(diff)

	// This assertion will now pass because the function will add all three colors.
	if !strings.Contains(colored, "\x1b[33m") || !strings.Contains(colored, "\x1b[31m") || !strings.Contains(colored, "\x1b[32m") {
		t.Fatalf("color codes missing: %q", colored)
	}
}

func TestEstimateDuration(t *testing.T) {
	tests := []models.TestResult{
		{TimeTaken: "1.5s"},
		{TimeTaken: "2.5s"},
	}
	dur := estimateDuration(tests)
	if dur < 4*time.Second || dur > 5*time.Second {
		t.Fatalf("unexpected duration: %v", dur)
	}
}

func TestParseTimeString(t *testing.T) {
	d, err := parseTimeString("1.5s")
	if err != nil || d != 1500*time.Millisecond {
		t.Fatalf("parseTimeString failed: %v, %v", d, err)
	}
}

func TestFmtDuration(t *testing.T) {
	res := fmtDuration(3*time.Second + 500*time.Millisecond)
	if !strings.Contains(res, "3.50") {
		t.Fatalf("unexpected fmtDuration: %q", res)
	}
}

func TestGenerateStatusAndHeadersTableDiff_NoDiff(t *testing.T) {
	test := models.TestResult{
		Result: models.Result{
			StatusCode:    models.IntResult{Normal: true},
			HeadersResult: []models.HeaderResult{},
			TrailerResult: []models.HeaderResult{},
			BodyResult:    []models.BodyResult{},
		},
	}
	diff := GenerateStatusAndHeadersTableDiff(test)
	if !strings.Contains(diff, "No differences") {
		t.Fatalf("expected no diff, got: %q", diff)
	}
}

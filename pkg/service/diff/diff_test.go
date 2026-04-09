package diff

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestComputeDiffIdenticalReports(t *testing.T) {
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusFailed},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusFailed},
		},
	}

	diff := ComputeDiff(report1, report2)
	if len(diff.Regressions) != 0 {
		t.Fatalf("expected no regressions, got %d", len(diff.Regressions))
	}
	if len(diff.Fixes) != 0 {
		t.Fatalf("expected no fixes, got %d", len(diff.Fixes))
	}
	if len(diff.Unchanged) != 2 {
		t.Fatalf("expected 2 unchanged test cases, got %d", len(diff.Unchanged))
	}
}

func TestComputeDiffRegressions(t *testing.T) {
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusPassed},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusFailed},
			{TestCaseID: "tc-2", Status: models.TestStatusPassed},
		},
	}

	diff := ComputeDiff(report1, report2)
	if len(diff.Regressions) != 1 {
		t.Fatalf("expected 1 regression, got %d", len(diff.Regressions))
	}
	if diff.Regressions[0].TestCaseID != "tc-1" {
		t.Fatalf("expected regression for tc-1, got %s", diff.Regressions[0].TestCaseID)
	}
	if len(diff.Fixes) != 0 {
		t.Fatalf("expected no fixes, got %d", len(diff.Fixes))
	}
	if len(diff.Unchanged) != 1 {
		t.Fatalf("expected 1 unchanged test case, got %d", len(diff.Unchanged))
	}
}

func TestComputeDiffFixes(t *testing.T) {
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusFailed},
			{TestCaseID: "tc-2", Status: models.TestStatusFailed},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusFailed},
		},
	}

	diff := ComputeDiff(report1, report2)
	if len(diff.Fixes) != 1 {
		t.Fatalf("expected 1 fix, got %d", len(diff.Fixes))
	}
	if diff.Fixes[0].TestCaseID != "tc-1" {
		t.Fatalf("expected fix for tc-1, got %s", diff.Fixes[0].TestCaseID)
	}
	if len(diff.Regressions) != 0 {
		t.Fatalf("expected no regressions, got %d", len(diff.Regressions))
	}
	if len(diff.Unchanged) != 1 {
		t.Fatalf("expected 1 unchanged test case, got %d", len(diff.Unchanged))
	}
}

func TestComputeDiffStatusTransitions(t *testing.T) {
	tests := []struct {
		name   string
		before models.TestStatus
		after  models.TestStatus
	}{
		{"IGNORED to PASSED", models.TestStatusIgnored, models.TestStatusPassed},
		{"PASSED to OBSOLETE", models.TestStatusPassed, models.TestStatusObsolete},
		{"IGNORED to FAILED", models.TestStatusIgnored, models.TestStatusFailed},
		{"FAILED to OBSOLETE", models.TestStatusFailed, models.TestStatusObsolete},
		{"OBSOLETE to PASSED", models.TestStatusObsolete, models.TestStatusPassed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report1 := &models.TestReport{
				Tests: []models.TestResult{
					{TestCaseID: "tc-1", Status: tc.before},
				},
			}
			report2 := &models.TestReport{
				Tests: []models.TestResult{
					{TestCaseID: "tc-1", Status: tc.after},
				},
			}

			diff := ComputeDiff(report1, report2)
			if len(diff.Regressions) != 0 {
				t.Fatalf("expected no regressions, got %d", len(diff.Regressions))
			}
			if len(diff.Fixes) != 0 {
				t.Fatalf("expected no fixes, got %d", len(diff.Fixes))
			}
			if len(diff.StatusTransitions) != 1 {
				t.Fatalf("expected 1 status transition, got %d", len(diff.StatusTransitions))
			}
			if diff.StatusTransitions[0].Before != tc.before || diff.StatusTransitions[0].After != tc.after {
				t.Fatalf("expected %s -> %s, got %s -> %s",
					tc.before, tc.after,
					diff.StatusTransitions[0].Before, diff.StatusTransitions[0].After)
			}
			if len(diff.Unchanged) != 0 {
				t.Fatalf("expected no unchanged, got %d", len(diff.Unchanged))
			}
		})
	}
}

func TestComputeDiffMixedChanges(t *testing.T) {
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusFailed},
			{TestCaseID: "tc-3", Status: models.TestStatusIgnored},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusFailed},
			{TestCaseID: "tc-2", Status: models.TestStatusPassed},
			{TestCaseID: "tc-3", Status: models.TestStatusIgnored},
		},
	}

	diff := ComputeDiff(report1, report2)
	if len(diff.Regressions) != 1 {
		t.Fatalf("expected 1 regression, got %d", len(diff.Regressions))
	}
	if len(diff.Fixes) != 1 {
		t.Fatalf("expected 1 fix, got %d", len(diff.Fixes))
	}
	if len(diff.Unchanged) != 1 {
		t.Fatalf("expected 1 unchanged test case, got %d", len(diff.Unchanged))
	}
}

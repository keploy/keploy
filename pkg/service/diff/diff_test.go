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

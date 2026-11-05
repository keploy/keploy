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

func TestComputeDiffReportsAddedTestCases(t *testing.T) {
	// #4583: a test case recorded after the first run appeared in no category
	// at all -- not regressions, not fixes, not transitions, not unchanged --
	// so `keploy diff` was silent about it.
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusPassed},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusPassed},
			{TestCaseID: "tc-3", Status: models.TestStatusFailed},
		},
	}

	diff := ComputeDiff(report1, report2)
	if len(diff.Added) != 1 {
		t.Fatalf("expected 1 added test case, got %d", len(diff.Added))
	}
	if diff.Added[0].TestCaseID != "tc-3" {
		t.Fatalf("expected tc-3 to be added, got %s", diff.Added[0].TestCaseID)
	}
	if diff.Added[0].After != models.TestStatusFailed {
		t.Fatalf("expected the added case to carry its new status, got %q", diff.Added[0].After)
	}
	if diff.Added[0].Before != StatusAbsent {
		t.Fatalf("expected no before-status for an added case, got %q", diff.Added[0].Before)
	}
	// An added case is not a transition, a regression or an unchanged one:
	// counting it twice would be as wrong as not counting it.
	if len(diff.Regressions) != 0 || len(diff.Fixes) != 0 || len(diff.StatusTransitions) != 0 {
		t.Fatalf("an added case leaked into the status categories: %+v", diff)
	}
	if len(diff.Unchanged) != 2 {
		t.Fatalf("expected the 2 common cases to stay unchanged, got %d", len(diff.Unchanged))
	}
}

func TestComputeDiffReportsRemovedTestCases(t *testing.T) {
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-2", Status: models.TestStatusFailed},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
		},
	}

	diff := ComputeDiff(report1, report2)
	if len(diff.Removed) != 1 {
		t.Fatalf("expected 1 removed test case, got %d", len(diff.Removed))
	}
	if diff.Removed[0].TestCaseID != "tc-2" {
		t.Fatalf("expected tc-2 to be removed, got %s", diff.Removed[0].TestCaseID)
	}
	if diff.Removed[0].Before != models.TestStatusFailed {
		t.Fatalf("expected the removed case to carry its last status, got %q", diff.Removed[0].Before)
	}
	if diff.Removed[0].After != StatusAbsent {
		t.Fatalf("expected no after-status for a removed case, got %q", diff.Removed[0].After)
	}
	if len(diff.Added) != 0 {
		t.Fatalf("expected nothing added, got %d", len(diff.Added))
	}
}

func TestComputeDiffAddedAndRemovedAreSortedAndDistinct(t *testing.T) {
	// Both directions at once, which is the shape a real run has: some cases
	// recorded, some deleted, some carried over.
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-9", Status: models.TestStatusPassed},
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-5", Status: models.TestStatusFailed},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
			{TestCaseID: "tc-7", Status: models.TestStatusPassed},
			{TestCaseID: "tc-3", Status: models.TestStatusPassed},
		},
	}

	diff := ComputeDiff(report1, report2)

	added := []string{}
	for _, change := range diff.Added {
		added = append(added, change.TestCaseID)
	}
	removed := []string{}
	for _, change := range diff.Removed {
		removed = append(removed, change.TestCaseID)
	}

	// Sorted, like commonIDs already is: the output is read by people, and an
	// order that depends on map iteration changes between runs.
	if len(added) != 2 || added[0] != "tc-3" || added[1] != "tc-7" {
		t.Fatalf("expected [tc-3 tc-7] added in order, got %v", added)
	}
	if len(removed) != 2 || removed[0] != "tc-5" || removed[1] != "tc-9" {
		t.Fatalf("expected [tc-5 tc-9] removed in order, got %v", removed)
	}
	if len(diff.Unchanged) != 1 || diff.Unchanged[0].TestCaseID != "tc-1" {
		t.Fatalf("expected only tc-1 unchanged, got %+v", diff.Unchanged)
	}
}

func TestComputeDiffIdenticalReportsAddNothing(t *testing.T) {
	// The accept control: two runs over the same cases must report neither an
	// addition nor a removal. Without it, "report the difference" could be
	// implemented by reporting everything.
	report := func() *models.TestReport {
		return &models.TestReport{
			Tests: []models.TestResult{
				{TestCaseID: "tc-1", Status: models.TestStatusPassed},
				{TestCaseID: "tc-2", Status: models.TestStatusFailed},
			},
		}
	}

	diff := ComputeDiff(report(), report())
	if len(diff.Added) != 0 || len(diff.Removed) != 0 {
		t.Fatalf("expected nothing added or removed, got %d added and %d removed",
			len(diff.Added), len(diff.Removed))
	}
}

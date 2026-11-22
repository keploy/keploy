package diff

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

func TestComputeDiffNilAndEmptyReports(t *testing.T) {
	populated := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-2", Status: models.TestStatusFailed},
			{TestCaseID: "tc-1", Status: models.TestStatusPassed},
		},
	}

	t.Run("both nil", func(t *testing.T) {
		got := ComputeDiff(nil, nil)
		if len(got.Added) != 0 || len(got.Removed) != 0 || len(got.Unchanged) != 0 ||
			len(got.Regressions) != 0 || len(got.Fixes) != 0 || len(got.StatusTransitions) != 0 {
			t.Fatalf("expected empty diff for nil reports, got %+v", got)
		}
	})

	t.Run("first nil reports all second tests as added", func(t *testing.T) {
		got := ComputeDiff(nil, populated)
		if len(got.Added) != 2 || got.Added[0].TestCaseID != "tc-1" || got.Added[1].TestCaseID != "tc-2" {
			t.Fatalf("expected [tc-1 tc-2] added, got %+v", got.Added)
		}
		if got.Added[0].Before != StatusAbsent || got.Added[0].After != models.TestStatusPassed {
			t.Fatalf("unexpected tc-1 added entry: %+v", got.Added[0])
		}
		if len(got.Removed) != 0 {
			t.Fatalf("expected no removed tests, got %+v", got.Removed)
		}
	})

	t.Run("second nil reports all first tests as removed", func(t *testing.T) {
		got := ComputeDiff(populated, nil)
		if len(got.Removed) != 2 || got.Removed[0].TestCaseID != "tc-1" || got.Removed[1].TestCaseID != "tc-2" {
			t.Fatalf("expected [tc-1 tc-2] removed, got %+v", got.Removed)
		}
		if got.Removed[1].Before != models.TestStatusFailed || got.Removed[1].After != StatusAbsent {
			t.Fatalf("unexpected tc-2 removed entry: %+v", got.Removed[1])
		}
		if len(got.Added) != 0 {
			t.Fatalf("expected no added tests, got %+v", got.Added)
		}
	})

	t.Run("empty vs populated", func(t *testing.T) {
		got := ComputeDiff(&models.TestReport{}, populated)
		if len(got.Added) != 2 || len(got.Removed) != 0 {
			t.Fatalf("expected 2 added and 0 removed, got %+v", got)
		}
	})
}

func TestComputeDiffAllCategoriesSimultaneously(t *testing.T) {
	report1 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-reg", Status: models.TestStatusPassed},
			{TestCaseID: "tc-fix", Status: models.TestStatusFailed},
			{TestCaseID: "tc-trans", Status: models.TestStatusIgnored},
			{TestCaseID: "tc-rem", Status: models.TestStatusFailed},
			{TestCaseID: "tc-same", Status: models.TestStatusPassed},
		},
	}
	report2 := &models.TestReport{
		Tests: []models.TestResult{
			{TestCaseID: "tc-reg", Status: models.TestStatusFailed},
			{TestCaseID: "tc-fix", Status: models.TestStatusPassed},
			{TestCaseID: "tc-trans", Status: models.TestStatusObsolete},
			{TestCaseID: "tc-add", Status: models.TestStatusPassed},
			{TestCaseID: "tc-same", Status: models.TestStatusPassed},
		},
	}

	got := ComputeDiff(report1, report2)
	if len(got.Regressions) != 1 || got.Regressions[0].TestCaseID != "tc-reg" {
		t.Fatalf("unexpected regressions: %+v", got.Regressions)
	}
	if len(got.Fixes) != 1 || got.Fixes[0].TestCaseID != "tc-fix" {
		t.Fatalf("unexpected fixes: %+v", got.Fixes)
	}
	if len(got.StatusTransitions) != 1 || got.StatusTransitions[0].TestCaseID != "tc-trans" {
		t.Fatalf("unexpected status transitions: %+v", got.StatusTransitions)
	}
	if len(got.Added) != 1 || got.Added[0].TestCaseID != "tc-add" || got.Added[0].Before != StatusAbsent || got.Added[0].After != models.TestStatusPassed {
		t.Fatalf("unexpected added: %+v", got.Added)
	}
	if len(got.Removed) != 1 || got.Removed[0].TestCaseID != "tc-rem" || got.Removed[0].Before != models.TestStatusFailed || got.Removed[0].After != StatusAbsent {
		t.Fatalf("unexpected removed: %+v", got.Removed)
	}
	if len(got.Unchanged) != 1 || got.Unchanged[0].TestCaseID != "tc-same" {
		t.Fatalf("unexpected unchanged: %+v", got.Unchanged)
	}
}

func TestWriteDiffIncludesAddedAndRemovedSections(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		var buf bytes.Buffer
		writeDiff(&buf, "run-1", "run-2", &DiffResult{
			Regressions: []StatusChange{{TestSet: "ts-1", TestCaseID: "tc-1", Before: models.TestStatusPassed, After: models.TestStatusFailed}},
			Added:       []StatusChange{{TestSet: "ts-2", TestCaseID: "tc-new", Before: StatusAbsent, After: models.TestStatusPassed}},
			Removed:     []StatusChange{{TestSet: "ts-1", TestCaseID: "tc-old", Before: models.TestStatusFailed, After: StatusAbsent}},
			Unchanged:   []StatusChange{{TestSet: "ts-1", TestCaseID: "tc-same", Before: models.TestStatusPassed, After: models.TestStatusPassed}},
		})
		out := buf.String()
		for _, want := range []string{
			"Added (only in the second run):\n  ts-2/tc-new: PASSED\n",
			"Removed (only in the first run):\n  ts-1/tc-old: was FAILED\n",
			"Summary: 1 regressions, 0 fixes, 0 status transitions, 1 added, 1 removed, 1 unchanged\n",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		var buf bytes.Buffer
		writeDiff(&buf, "run-1", "run-2", &DiffResult{})
		out := buf.String()
		for _, want := range []string{
			"Added (only in the second run):\n  none\n",
			"Removed (only in the first run):\n  none\n",
			"Summary: 0 regressions, 0 fixes, 0 status transitions, 0 added, 0 removed, 0 unchanged\n",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("output missing %q:\n%s", want, out)
			}
		}
	})
}

type stubReportDB struct {
	reports map[string]*models.TestReport
}

func (s *stubReportDB) GetReport(_ context.Context, runID, testSetID string) (*models.TestReport, error) {
	rep, ok := s.reports[runID+"/"+testSetID]
	if !ok {
		return nil, fmt.Errorf("missing report %s/%s", runID, testSetID)
	}
	return rep, nil
}

type stubTestDB struct {
	sets map[string][]string
}

func (s *stubTestDB) GetReportTestSets(_ context.Context, runID string) ([]string, error) {
	return s.sets[runID], nil
}

func TestCompareAggregatesAddedAndRemovedAcrossTestSets(t *testing.T) {
	rdb := &stubReportDB{
		reports: map[string]*models.TestReport{
			"run-1/ts-b": {Tests: []models.TestResult{{TestCaseID: "tc-1", Status: models.TestStatusPassed}, {TestCaseID: "tc-removed", Status: models.TestStatusFailed}}},
			"run-2/ts-b": {Tests: []models.TestResult{{TestCaseID: "tc-1", Status: models.TestStatusPassed}}},
			"run-1/ts-a": {Tests: []models.TestResult{{TestCaseID: "tc-1", Status: models.TestStatusPassed}}},
			"run-2/ts-a": {Tests: []models.TestResult{{TestCaseID: "tc-1", Status: models.TestStatusPassed}, {TestCaseID: "tc-added", Status: models.TestStatusFailed}}},
		},
	}
	tdb := &stubTestDB{
		sets: map[string][]string{
			"run-1": {"ts-b", "ts-a"},
			"run-2": {"ts-a", "ts-b"},
		},
	}

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = r.Close()
		_ = w.Close()
	})

	svc := New(zap.NewNop(), rdb, tdb)
	compareErr := svc.Compare(context.Background(), "run-1", "run-2", nil)
	_ = w.Close()
	outBytes, readErr := io.ReadAll(r)
	os.Stdout = origStdout
	if compareErr != nil {
		t.Fatalf("Compare returned error: %v", compareErr)
	}
	if readErr != nil {
		t.Fatalf("failed reading stdout: %v", readErr)
	}
	out := string(outBytes)
	for _, want := range []string{
		"Added (only in the second run):\n  ts-a/tc-added: FAILED\n",
		"Removed (only in the first run):\n  ts-b/tc-removed: was FAILED\n",
		"Summary: 0 regressions, 0 fixes, 0 status transitions, 1 added, 1 removed, 2 unchanged\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Compare output missing %q:\n%s", want, out)
		}
	}
}

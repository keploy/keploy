package diff

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/report"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type Diff struct {
	logger   *zap.Logger
	reportDB ReportDB
	testDB   TestDB
}

type StatusChange struct {
	TestSet    string
	TestCaseID string
	Before     models.TestStatus
	After      models.TestStatus
}

type DiffResult struct {
	Regressions       []StatusChange
	Fixes             []StatusChange
	StatusTransitions []StatusChange
	Unchanged         []StatusChange
}

func New(logger *zap.Logger, reportDB ReportDB, testDB TestDB) *Diff {
	return &Diff{
		logger:   logger,
		reportDB: reportDB,
		testDB:   testDB,
	}
}

func ComputeDiff(report1, report2 *models.TestReport) *DiffResult {
	result := &DiffResult{
		Regressions:       make([]StatusChange, 0),
		Fixes:             make([]StatusChange, 0),
		StatusTransitions: make([]StatusChange, 0),
		Unchanged:         make([]StatusChange, 0),
	}
	if report1 == nil && report2 == nil {
		return result
	}

	left := make(map[string]models.TestStatus)
	if report1 != nil {
		for _, test := range report1.Tests {
			id := strings.TrimSpace(test.TestCaseID)
			if id != "" {
				left[id] = test.Status
			}
		}
	}

	right := make(map[string]models.TestStatus)
	if report2 != nil {
		for _, test := range report2.Tests {
			id := strings.TrimSpace(test.TestCaseID)
			if id != "" {
				right[id] = test.Status
			}
		}
	}

	allIDsMap := make(map[string]struct{}, len(left)+len(right))
	for id := range left {
		allIDsMap[id] = struct{}{}
	}
	for id := range right {
		allIDsMap[id] = struct{}{}
	}

	allIDs := make([]string, 0, len(allIDsMap))
	for id := range allIDsMap {
		allIDs = append(allIDs, id)
	}
	sort.Strings(allIDs)

	for _, id := range allIDs {
		before, inLeft := left[id]
		after, inRight := right[id]

		if !inLeft {
			before = "ABSENT"
		}
		if !inRight {
			after = "ABSENT"
		}

		change := StatusChange{
			TestCaseID: id,
			Before:     before,
			After:      after,
		}
		switch {
		case inLeft && inRight && before == after:
			result.Unchanged = append(result.Unchanged, change)
		case inLeft && inRight && before == models.TestStatusPassed && after == models.TestStatusFailed:
			result.Regressions = append(result.Regressions, change)
		case inLeft && inRight && before == models.TestStatusFailed && after == models.TestStatusPassed:
			result.Fixes = append(result.Fixes, change)
		default:
			// Non-binary transitions such as IGNORED->PASSED, PASSED->OBSOLETE, ABSENT->PASSED, PASSED->ABSENT, etc.
			result.StatusTransitions = append(result.StatusTransitions, change)
		}
	}

	return result
}

func (d *Diff) Compare(ctx context.Context, run1 string, run2 string, testSets []string) error {
	selectedTestSets, err := d.resolveTestSets(ctx, run1, run2, testSets)
	if err != nil {
		return err
	}

	aggregate := &DiffResult{
		Regressions:       make([]StatusChange, 0),
		Fixes:             make([]StatusChange, 0),
		StatusTransitions: make([]StatusChange, 0),
		Unchanged:         make([]StatusChange, 0),
	}

	for _, testSetID := range selectedTestSets {
		report1, err1 := d.reportDB.GetReport(ctx, run1, testSetID)
		report2, err2 := d.reportDB.GetReport(ctx, run2, testSetID)
		if err1 != nil && err2 != nil {
			return fmt.Errorf("%s failed to load report for test-set %q in both runs %q and %q", utils.Emoji, testSetID, run1, run2)
		}

		diff := ComputeDiff(report1, report2)
		aggregate.Regressions = append(aggregate.Regressions, withTestSet(testSetID, diff.Regressions)...)
		aggregate.Fixes = append(aggregate.Fixes, withTestSet(testSetID, diff.Fixes)...)
		aggregate.StatusTransitions = append(aggregate.StatusTransitions, withTestSet(testSetID, diff.StatusTransitions)...)
		aggregate.Unchanged = append(aggregate.Unchanged, withTestSet(testSetID, diff.Unchanged)...)
	}

	printDiff(run1, run2, aggregate)
	return nil
}

func (d *Diff) resolveTestSets(ctx context.Context, run1 string, run2 string, testSets []string) ([]string, error) {
	if len(testSets) > 0 {
		normalized := normalizeTestSets(testSets)
		if len(normalized) == 0 {
			return nil, fmt.Errorf("%s no valid test-sets were provided", utils.Emoji)
		}
		return normalized, nil
	}

	run1Sets, err := d.testDB.GetReportTestSets(ctx, run1)
	if err != nil {
		return nil, fmt.Errorf("%s failed to get test-sets for run %q: %w", utils.Emoji, run1, err)
	}
	run2Sets, err := d.testDB.GetReportTestSets(ctx, run2)
	if err != nil {
		return nil, fmt.Errorf("%s failed to get test-sets for run %q: %w", utils.Emoji, run2, err)
	}

	allSetsMap := make(map[string]struct{})
	for _, setID := range normalizeTestSets(run1Sets) {
		allSetsMap[setID] = struct{}{}
	}
	for _, setID := range normalizeTestSets(run2Sets) {
		allSetsMap[setID] = struct{}{}
	}
	allSets := make([]string, 0, len(allSetsMap))
	for setID := range allSetsMap {
		allSets = append(allSets, setID)
	}
	sort.Strings(allSets)
	if len(allSets) == 0 {
		return nil, fmt.Errorf("%s no test-sets found for %q or %q", utils.Emoji, run1, run2)
	}
	return allSets, nil
}

func normalizeTestSets(testSets []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(testSets))
	for _, setID := range testSets {
		trimmed := strings.TrimSpace(setID)
		trimmed = strings.TrimSuffix(trimmed, report.ReportSuffix)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	sort.Strings(out)
	return out
}

func withTestSet(testSetID string, changes []StatusChange) []StatusChange {
	out := make([]StatusChange, len(changes))
	for i, change := range changes {
		change.TestSet = testSetID
		out[i] = change
	}
	return out
}

func printDiff(run1 string, run2 string, result *DiffResult) {
	fmt.Fprintf(os.Stdout, "Test Run Comparison: %s vs %s\n\n", run1, run2)

	fmt.Fprintln(os.Stdout, "Regressions (newly failing):")
	if len(result.Regressions) == 0 {
		fmt.Fprintln(os.Stdout, "  none")
	} else {
		for _, change := range result.Regressions {
			fmt.Fprintf(os.Stdout, "  %s: %s -> %s\n", formatTestCaseLabel(change), change.Before, change.After)
		}
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Fixes (newly passing):")
	if len(result.Fixes) == 0 {
		fmt.Fprintln(os.Stdout, "  none")
	} else {
		for _, change := range result.Fixes {
			fmt.Fprintf(os.Stdout, "  %s: %s -> %s\n", formatTestCaseLabel(change), change.Before, change.After)
		}
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Status transitions (other changes):")
	if len(result.StatusTransitions) == 0 {
		fmt.Fprintln(os.Stdout, "  none")
	} else {
		for _, change := range result.StatusTransitions {
			fmt.Fprintf(os.Stdout, "  %s: %s -> %s\n", formatTestCaseLabel(change), change.Before, change.After)
		}
	}

	fmt.Fprintf(os.Stdout, "\nSummary: %d regressions, %d fixes, %d status transitions, %d unchanged\n", len(result.Regressions), len(result.Fixes), len(result.StatusTransitions), len(result.Unchanged))
}

func formatTestCaseLabel(change StatusChange) string {
	if change.TestSet == "" {
		return change.TestCaseID
	}
	return fmt.Sprintf("%s/%s", change.TestSet, change.TestCaseID)
}

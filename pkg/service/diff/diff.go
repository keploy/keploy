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
	// Added holds test cases present in the second run only; Removed, those
	// present in the first only. Before ComputeDiff looked at them, a test
	// case added or deleted between two runs appeared in no category at all
	// (#4583). The side that does not exist carries the empty status, which
	// is what StatusAbsent names.
	Added     []StatusChange
	Removed   []StatusChange
	Unchanged []StatusChange
}

// StatusAbsent is the Before of an added test case and the After of a removed
// one: the run in question has no status for it, which is not the same as any
// status it could have had.
const StatusAbsent models.TestStatus = ""

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
		Added:             make([]StatusChange, 0),
		Removed:           make([]StatusChange, 0),
		Unchanged:         make([]StatusChange, 0),
	}
	if report1 == nil || report2 == nil {
		return result
	}

	left := make(map[string]models.TestStatus, len(report1.Tests))
	for _, test := range report1.Tests {
		id := strings.TrimSpace(test.TestCaseID)
		if id == "" {
			continue
		}
		left[id] = test.Status
	}

	right := make(map[string]models.TestStatus, len(report2.Tests))
	for _, test := range report2.Tests {
		id := strings.TrimSpace(test.TestCaseID)
		if id == "" {
			continue
		}
		right[id] = test.Status
	}

	commonIDs := make([]string, 0, len(left))
	removedIDs := make([]string, 0)
	for id := range left {
		if _, ok := right[id]; ok {
			commonIDs = append(commonIDs, id)
			continue
		}
		removedIDs = append(removedIDs, id)
	}
	sort.Strings(commonIDs)
	sort.Strings(removedIDs)

	addedIDs := make([]string, 0)
	for id := range right {
		if _, ok := left[id]; !ok {
			addedIDs = append(addedIDs, id)
		}
	}
	sort.Strings(addedIDs)

	for _, id := range commonIDs {
		before := left[id]
		after := right[id]
		change := StatusChange{
			TestCaseID: id,
			Before:     before,
			After:      after,
		}
		switch {
		case before == after:
			result.Unchanged = append(result.Unchanged, change)
		case before == models.TestStatusPassed && after == models.TestStatusFailed:
			result.Regressions = append(result.Regressions, change)
		case before == models.TestStatusFailed && after == models.TestStatusPassed:
			result.Fixes = append(result.Fixes, change)
		default:
			// Non-binary transitions such as IGNORED->PASSED, PASSED->OBSOLETE, etc.
			result.StatusTransitions = append(result.StatusTransitions, change)
		}
	}

	for _, id := range addedIDs {
		result.Added = append(result.Added, StatusChange{
			TestCaseID: id,
			Before:     StatusAbsent,
			After:      right[id],
		})
	}

	for _, id := range removedIDs {
		result.Removed = append(result.Removed, StatusChange{
			TestCaseID: id,
			Before:     left[id],
			After:      StatusAbsent,
		})
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
		Added:             make([]StatusChange, 0),
		Removed:           make([]StatusChange, 0),
		Unchanged:         make([]StatusChange, 0),
	}

	for _, testSetID := range selectedTestSets {
		report1, err := d.reportDB.GetReport(ctx, run1, testSetID)
		if err != nil {
			return fmt.Errorf("%s failed to load report for run %q and test-set %q: %w", utils.Emoji, run1, testSetID, err)
		}

		report2, err := d.reportDB.GetReport(ctx, run2, testSetID)
		if err != nil {
			return fmt.Errorf("%s failed to load report for run %q and test-set %q: %w", utils.Emoji, run2, testSetID, err)
		}

		diff := ComputeDiff(report1, report2)
		aggregate.Regressions = append(aggregate.Regressions, withTestSet(testSetID, diff.Regressions)...)
		aggregate.Fixes = append(aggregate.Fixes, withTestSet(testSetID, diff.Fixes)...)
		aggregate.StatusTransitions = append(aggregate.StatusTransitions, withTestSet(testSetID, diff.StatusTransitions)...)
		aggregate.Added = append(aggregate.Added, withTestSet(testSetID, diff.Added)...)
		aggregate.Removed = append(aggregate.Removed, withTestSet(testSetID, diff.Removed)...)
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

	run1SetMap := make(map[string]struct{})
	for _, setID := range normalizeTestSets(run1Sets) {
		run1SetMap[setID] = struct{}{}
	}
	common := make([]string, 0)
	for _, setID := range normalizeTestSets(run2Sets) {
		if _, ok := run1SetMap[setID]; ok {
			common = append(common, setID)
		}
	}
	sort.Strings(common)
	if len(common) == 0 {
		return nil, fmt.Errorf("%s no common test-sets found between %q and %q", utils.Emoji, run1, run2)
	}
	return common, nil
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

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Added (only in the second run):")
	if len(result.Added) == 0 {
		fmt.Fprintln(os.Stdout, "  none")
	} else {
		for _, change := range result.Added {
			fmt.Fprintf(os.Stdout, "  %s: %s\n", formatTestCaseLabel(change), change.After)
		}
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Removed (only in the first run):")
	if len(result.Removed) == 0 {
		fmt.Fprintln(os.Stdout, "  none")
	} else {
		for _, change := range result.Removed {
			fmt.Fprintf(os.Stdout, "  %s: was %s\n", formatTestCaseLabel(change), change.Before)
		}
	}

	fmt.Fprintf(os.Stdout, "\nSummary: %d regressions, %d fixes, %d status transitions, %d added, %d removed, %d unchanged\n", len(result.Regressions), len(result.Fixes), len(result.StatusTransitions), len(result.Added), len(result.Removed), len(result.Unchanged))
}

func formatTestCaseLabel(change StatusChange) string {
	if change.TestSet == "" {
		return change.TestCaseID
	}
	return fmt.Sprintf("%s/%s", change.TestSet, change.TestCaseID)
}

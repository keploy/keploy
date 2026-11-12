package replay

import (
	"context"
	"path/filepath"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// missingSelected is the test cases the run selected in a test set by name
// (test.selectedTests in keploy.yml, or the enterprise binary's -t set:case)
// that are not on disk: no test case of the set is named so. Each is listed
// once, in the order selected. A run that selects no test case of the set
// runs every one on disk, and misses none. selected is how many distinct
// names it selected.
//
// The set's Total counts every name selected, so each must get a verdict: one
// not on disk used to get no row at all, and the set passed with it (Total
// 3, two rows, PASSED).
func missingSelected(names []string, testCases []*models.TestCase) (missing []string, selected int) {
	onDisk := make(map[string]bool, len(testCases))
	for _, tc := range testCases {
		onDisk[tc.Name] = true
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !onDisk[name] {
			missing = append(missing, name)
		}
	}
	return missing, len(seen)
}

// failedForNotFoundAlone reports whether a set is FAILED only for the test
// cases it selected that are not on disk (notFound of them): failure, which
// counts their rows with every other failed test of the set, has none
// besides them, and neither mock.real (realWhy) nor the calls no mock
// answered (unclaimedWhy) failed it. Such a set is checked as it would be
// without them: its worker scope is waited for and judged (the proven
// switch), and one that stopped early is the partial run it would have been:
// APP_FAULT, with its application's logs, and off docker compose the
// remaining test sets are not run, as its stop is not one this set alone is
// likely to have.
func failedForNotFoundAlone(status models.TestSetStatus, failure, notFound int, realWhy, unclaimedWhy string) bool {
	return status == models.TestSetStatusFailed && notFound > 0 && failure == notFound && realWhy == "" && unclaimedWhy == ""
}

// notFoundReason is why a selected test case not on disk fails.
func notFoundReason(testSetID, name string) string {
	return "test case " + name + " not found in " + testSetID
}

// notFoundSetReason is the failure reason of a set some selected test cases
// of which are not on disk.
func notFoundSetReason(testSetID string, missing []string) string {
	if len(missing) == 1 {
		return notFoundReason(testSetID, missing[0])
	}
	return "test cases " + firstNames(missing, 10) + " not found in " + testSetID
}

// notFoundResult is the FAILED row of a selected test case that is not on
// disk. It was never sent: it has no request and no answer, only why
// (FailureInfo.NotFound), filed under TEST_CASE_NOT_FOUND and graded HIGH, as
// a test case refused when its set loaded is.
func (r *Replayer) notFoundResult(testSetID, name string) *models.TestResult {
	now := time.Now().UTC()
	res := &models.TestResult{
		Name:         testSetID,
		Status:       models.TestStatusFailed,
		Started:      now.Unix(),
		Completed:    now.Unix(),
		TestCaseID:   name,
		TestCasePath: filepath.Join(r.config.Path, testSetID),
		MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
		TimeTaken:    "0s",
		Result: models.Result{
			StatusCode:    models.IntResult{Normal: true},
			HeadersResult: []models.HeaderResult{},
			BodyResult:    []models.BodyResult{},
		},
	}
	// Result.FailureInfo.Risk is what the set's HighRisk counts; the row's
	// own FailureInfo is what is persisted, and what keploy report reads.
	res.Result.FailureInfo.Risk = models.High
	res.FailureInfo.Risk = models.High
	res.FailureInfo.Category = []models.FailureCategory{models.TestCaseNotFound}
	res.FailureInfo.NotFound = notFoundReason(testSetID, name)
	return res
}

// failMissingSelected writes the FAILED row of each selected test case of the
// set that is not on disk (missingSelected), and says once which they are and
// what to do. The rows are the set's verdicts on them: the set fails.
func (r *Replayer) failMissingSelected(ctx context.Context, testRunID, testSetID string, missing []string) ([]models.TestResult, error) {
	utils.LogError(r.logger, nil, "test cases selected for this run are not in their test set: each fails, unsent, and so does the set",
		zap.String("testSet", testSetID),
		zap.Strings("notFound", missing),
		zap.String("next_step", "select only recorded test cases (in test.selectedTests in keploy.yml, or in -t set:case with the enterprise binary), or record the missing ones with keploy record"))
	rows := make([]models.TestResult, 0, len(missing))
	for _, name := range missing {
		res := r.notFoundResult(testSetID, name)
		if err := r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, res); err != nil {
			utils.LogError(r.logger, err, "failed to insert test case result for a selected test case that is not on disk")
			return nil, err
		}
		rows = append(rows, *res)
	}
	return rows, nil
}

// notFoundUnsent reports whether res is a selected test case that is not on
// disk: it was never sent, and there is no test case to remove or mend.
func notFoundUnsent(res models.TestResult) bool {
	return res.FailureInfo.NotFound != ""
}

// dropDeletedFromSelection takes the test cases --must-pass removed from a
// set out of the set's selection, so the next attempt runs what is left of
// it. It reports whether the set has anything left to run: false when the
// removed test cases were all it selected. That selection is left as it was,
// never emptied: an empty one runs every test case on disk.
func (r *Replayer) dropDeletedFromSelection(testSetID string, deleted []string) bool {
	selected := r.config.Test.SelectedTests[testSetID]
	if len(selected) == 0 {
		return true
	}
	gone := make(map[string]bool, len(deleted))
	for _, name := range deleted {
		gone[name] = true
	}
	left := make([]string, 0, len(selected))
	for _, name := range selected {
		if !gone[name] {
			left = append(left, name)
		}
	}
	if len(left) == 0 {
		return false
	}
	r.config.Test.SelectedTests[testSetID] = left
	return true
}

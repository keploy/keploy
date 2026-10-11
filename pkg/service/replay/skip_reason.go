package replay

import (
	"path/filepath"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// skipReasonFor is the reason the run skips one test, for its IGNORED result:
// what config.Test.SkipReasons holds for it, under the proven switch. With the
// switch off it is always "", so an IGNORED result is what it was before.
func (r *Replayer) skipReasonFor(testSetID, testCaseID string) string {
	if !provenswitch.On() {
		return ""
	}
	return r.config.Test.SkipReasons[testSetID][testCaseID]
}

// ignoredSetResults is one IGNORED result, with its reason, for each test of a
// test set the run ignores whole, under the proven switch. A set's skip used
// to leave its report with counts and no rows, so neither the report nor
// JUnit could say which tests were skipped, or why. The run's own selection
// still decides which tests exist: a test it did not select is not reported
// as skipped.
//
// With the switch off it returns nil, and the set's report is what it was.
func (r *Replayer) ignoredSetResults(testSetID string, testCases []*models.TestCase) []models.TestResult {
	if !provenswitch.On() {
		return nil
	}
	selected := r.config.Test.SelectedTests[testSetID]
	isSelected := make(map[string]bool, len(selected))
	for _, name := range selected {
		isSelected[name] = true
	}
	out := make([]models.TestResult, 0, len(testCases))
	for _, tc := range testCases {
		if tc == nil || (len(selected) > 0 && !isSelected[tc.Name]) {
			continue
		}
		out = append(out, models.TestResult{
			Kind:         tc.Kind,
			Name:         testSetID,
			Status:       models.TestStatusIgnored,
			TestCaseID:   tc.Name,
			TestCasePath: filepath.Join(r.config.Path, testSetID),
			MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
			SkipReason:   r.skipReasonFor(testSetID, tc.Name),
		})
	}
	return out
}

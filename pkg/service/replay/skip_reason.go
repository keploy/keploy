package replay

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// skipReasonFor is the reason the run skips one test, for its IGNORED result:
// what config.Test.SkipReasons holds for it, whoever set it, with the proven
// switch on or off. A test skipped for a reason is said as it is skipped (at
// Debug), once a run, and is listed in the run's summary (quarantinedSummary).
// A test skipped with no reason gets "", and nothing is said, so a run whose
// caller sets no reason (every run with the switch off, but a cloud replay
// that skips a test for having no mapping entry) is what it was.
func (r *Replayer) skipReasonFor(testSetID, testCaseID string) string {
	reason := r.config.Test.SkipReasons[testSetID][testCaseID]
	if reason != "" {
		r.noteQuarantined(testSetID, testCaseID, reason)
	}
	return reason
}

// noteQuarantined says, at Debug, that the run skips a test for a reason, the
// first time it does in this run (a flaky-check attempt runs a set again), and
// keeps it for the run's summary, which lists it.
func (r *Replayer) noteQuarantined(testSetID, testCaseID, reason string) {
	r.quarantinedMu.Lock()
	defer r.quarantinedMu.Unlock()
	key := [2]string{testSetID, testCaseID}
	if _, said := r.quarantined[key]; said {
		return
	}
	if r.quarantined == nil {
		r.quarantined = make(map[[2]string]string)
	}
	r.quarantined[key] = reason
	msg := "skipping a test for a reason"
	if provenswitch.On() {
		msg = "skipping a quarantined test (" + provenswitch.Env + "=1)"
	}
	r.logger.Debug(msg,
		zap.String("testcase", testCaseID),
		zap.String("testset", testSetID),
		zap.String("reason", reason))
}

// forgetQuarantined starts a run with no quarantined tests.
func (r *Replayer) forgetQuarantined() {
	r.quarantinedMu.Lock()
	r.quarantined = nil
	r.quarantinedMu.Unlock()
}

// quarantinedSummary is the run summary's list of the tests it skipped for a
// reason, by test set and test, each with its reason on one line; "" when it
// skipped none. Its heading names the proven switch only when the switch is on.
func (r *Replayer) quarantinedSummary() string {
	r.quarantinedMu.Lock()
	keys := make([][2]string, 0, len(r.quarantined))
	for k := range r.quarantined {
		keys = append(keys, k)
	}
	reasons := make(map[[2]string]string, len(keys))
	for _, k := range keys {
		reasons[k] = r.quarantined[k]
	}
	r.quarantinedMu.Unlock()
	if len(keys) == 0 {
		return ""
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	var b strings.Builder
	if provenswitch.On() {
		fmt.Fprintf(&b, "\n\n\tQuarantined tests, skipped by keploy (%s=1): %d\n", provenswitch.Env, len(keys))
	} else {
		fmt.Fprintf(&b, "\n\n\tTests skipped by keploy for a reason: %d\n", len(keys))
	}
	for _, k := range keys {
		fmt.Fprintf(&b, "\t\t%s/%s: %s\n", k[0], k[1], models.SkipReasonOneLine(reasons[k]))
	}
	return b.String()
}

// ignoredKind is the Kind of an IGNORED result: the test's own under the proven
// switch, and HTTP, whatever the test is, with it off, as before.
func ignoredKind(tc *models.TestCase) models.Kind {
	if provenswitch.On() && tc != nil {
		return tc.Kind
	}
	return models.HTTP
}

// ignoredSetResults is one IGNORED result, with its reason, for each test of a
// test set the run ignores whole, under the proven switch, or with it off when
// the run's caller gave a reason for any test of the set (config.Test.SkipReasons:
// a cloud replay that skips a set's tests for having no mapping entry). A set's
// skip used to leave its report with counts and no rows, so neither the report
// nor JUnit could say which tests were skipped, or why. The run's own
// selection still decides which tests exist: a test it did not select is not
// reported as skipped.
//
// With the switch off and no reason for the set it returns nil, and the set's
// report is what it was.
func (r *Replayer) ignoredSetResults(testSetID string, testCases []*models.TestCase) []models.TestResult {
	if !provenswitch.On() && len(r.config.Test.SkipReasons[testSetID]) == 0 {
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
			Kind:         ignoredKind(tc),
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

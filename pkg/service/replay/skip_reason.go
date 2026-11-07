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

// skipKindFor is the kind of the skip skipReasonFor gives a reason for, for
// its IGNORED result: what config.Test.SkipKinds holds for the test, and
// SkipQuarantined for a skip whose caller names no kind and for a test the run
// skips with no reason, which no kind is said for.
func (r *Replayer) skipKindFor(testSetID, testCaseID string) models.SkipKind {
	if r.config.Test.SkipReasons[testSetID][testCaseID] == "" {
		return models.SkipQuarantined
	}
	return r.config.Test.SkipKinds[testSetID][testCaseID]
}

// skipNote is a test the run skipped for a reason: the reason, and the kind
// of the skip.
type skipNote struct {
	reason string
	kind   models.SkipKind
}

// noteQuarantined says, at Debug, that the run skips a test for a reason, the
// first time it does in this run (a flaky-check attempt runs a set again), and
// keeps it, with the skip's kind, for the run's summary, which lists it.
func (r *Replayer) noteQuarantined(testSetID, testCaseID, reason string) {
	r.quarantinedMu.Lock()
	defer r.quarantinedMu.Unlock()
	key := [2]string{testSetID, testCaseID}
	if _, said := r.quarantined[key]; said {
		return
	}
	if r.quarantined == nil {
		r.quarantined = make(map[[2]string]skipNote)
	}
	kind := r.skipKindFor(testSetID, testCaseID)
	r.quarantined[key] = skipNote{reason: reason, kind: kind}
	msg, _ := skipWords(kind)
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

// skipWords are what the run says of a skip of kind: at Debug, as it skips
// the test, and as the heading of the run summary's block of such skips. A
// skip that is no quarantine is not called one. A quarantine, and a kind
// keploy does not know, get a quarantine's words, which name the proven
// switch only when the switch is on, as they did before skips had kinds.
func skipWords(kind models.SkipKind) (said, heading string) {
	switch kind {
	case models.SkipThisRunOnly:
		return "skipping a test for this run only", "Tests skipped by keploy for this run only, not quarantined"
	case models.SkipUntilRerecorded:
		return "skipping a test until it is re-recorded", "Tests skipped by keploy until they are re-recorded, not quarantined"
	}
	if provenswitch.On() {
		return "skipping a quarantined test (" + provenswitch.Env + "=1)", "Quarantined tests, skipped by keploy (" + provenswitch.Env + "=1)"
	}
	return "skipping a test for a reason", "Tests skipped by keploy for a reason"
}

// quarantinedSummary is the run summary's list of the tests it skipped for a
// reason, one block per kind of skip (models.SkipKinds, the quarantined
// first), each under its heading (skipWords) and by test set and test, each
// test with its reason on one line; "" when it skipped none. A kind keploy
// does not know is listed with the quarantined (models.SkipKind.Said), and a
// run that skipped only quarantined tests prints what it printed before
// skips had kinds.
func (r *Replayer) quarantinedSummary() string {
	r.quarantinedMu.Lock()
	byKind := make(map[models.SkipKind][][2]string)
	reasons := make(map[[2]string]string, len(r.quarantined))
	for k, n := range r.quarantined {
		reasons[k] = n.reason
		kind := n.kind.Said()
		byKind[kind] = append(byKind[kind], k)
	}
	r.quarantinedMu.Unlock()
	if len(reasons) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, kind := range models.SkipKinds() {
		keys := byKind[kind]
		if len(keys) == 0 {
			continue
		}
		_, heading := skipWords(kind)
		fmt.Fprintf(&b, "\n\t%s: %d\n", heading, len(keys))
		writeSkipLines(&b, keys, reasons)
	}
	return b.String()
}

// writeSkipLines writes one line a test, by test set and test: the test and
// its reason on one line.
func writeSkipLines(b *strings.Builder, keys [][2]string, reasons map[[2]string]string) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(b, "\t\t%s/%s: %s\n", k[0], k[1], models.SkipReasonOneLine(reasons[k]))
	}
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
			SkipKind:     r.skipKindFor(testSetID, tc.Name),
		})
	}
	return out
}

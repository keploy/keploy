package replay

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// markUnmatchedCallsUnread records on a test's result that its unmatched calls
// could not all be read.
func markUnmatchedCallsUnread(result *models.TestResult) {
	if result != nil {
		result.FailureInfo.UnmatchedCallsUnread = true
	}
}

// testsWithUnreadMisses names the tests of a set whose unmatched calls could
// not all be read (FailureInfo.UnmatchedCallsUnread), in result order. Under
// the proven switch a miss fails its test, so such a test has no verdict and
// neither has its set: RunTestSet reports the set INTERNAL_ERR, which is
// neither PASSED nor FAILED, and nothing reading the report can take it for
// either. With the switch off it is always nil.
func testsWithUnreadMisses(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var out []string
	for _, r := range results {
		if r.FailureInfo.UnmatchedCallsUnread {
			out = append(out, r.TestCaseID)
		}
	}
	return out
}

// unreadMissesReason is the test-set report's reason for a set left unjudged
// by unread misses.
func unreadMissesReason(tests []string) string {
	return fmt.Sprintf("not judged: the calls that %d test(s) made with no recorded answer could not all be read from keploy's agent (%s); check the agent's log for why, then run the test-set again",
		len(tests), strings.Join(tests, ", "))
}

// markNotJudged records that testSetID ended not judged: its INTERNAL_ERR
// fails the set and the run, and does not end the round (abortAfterTestSet).
func (r *Replayer) markNotJudged(testSetID string) {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	if r.notJudgedSets == nil {
		r.notJudgedSets = map[string]bool{}
	}
	r.notJudgedSets[testSetID] = true
}

// notJudged reports whether testSetID ended not judged in this run.
func (r *Replayer) notJudged(testSetID string) bool {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	return r.notJudgedSets[testSetID]
}

// forgetNotJudged clears testSetID's mark, as a run of the set starts.
func (r *Replayer) forgetNotJudged(testSetID string) {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	delete(r.notJudgedSets, testSetID)
}

// testsWithUnreadCapture names the tests of a set whose capture could not be
// told (FailureInfo.CaptureUnread: keploy could not read its agent's capture-
// loss counts as the test ended, or at its set's mark), in result order. Under
// the proven switch such a test has no verdict and neither has its set. With
// the switch off it is always nil.
func testsWithUnreadCapture(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var out []string
	for _, r := range results {
		if r.FailureInfo.CaptureUnread {
			out = append(out, r.TestCaseID)
		}
	}
	return out
}

// unreadCaptureReason is the test-set report's reason for a set left unjudged
// by tests whose capture could not be told.
func unreadCaptureReason(tests []string) string {
	return fmt.Sprintf("not judged: keploy could not read whether its agent captured every call while %d test(s) ran (%s); check the agent's log for why its count could not be read, then run the test-set again",
		len(tests), strings.Join(tests, ", "))
}

// testsKeployCannotCapture names, under the proven switch, the tests of a set
// during or after which the application called what keploy cannot capture at
// all (FailureInfo.CannotCapture, decision 20: a Unix socket), or during which
// a client abandoned its TLS handshake without a word, and says what: they
// have no verdict, and the set is not judged. Never a failure.
func testsKeployCannotCapture(results []models.TestResult) ([]string, models.CaptureLosses) {
	if !provenswitch.On() {
		return nil, nil
	}
	var out []string
	var standing, own models.CaptureLosses
	for _, r := range results {
		if len(r.FailureInfo.CannotCapture) == 0 {
			continue
		}
		out = append(out, r.TestCaseID)
		var latest models.CaptureLosses
		for _, c := range r.FailureInfo.CannotCapture {
			if c.Kind.PerTest() {
				// A test's own (an abandoned TLS handshake): each
				// test's, so the set's is their sum.
				own = own.Plus(models.CaptureLosses{c})
			} else {
				latest = append(latest, c)
			}
		}
		if len(latest) > 0 {
			standing = latest // the latest test's: these counts only grow
		}
	}
	return out, standing.Plus(own)
}

// notCapturableReason is the test-set report's reason for a set left unjudged
// by tests whose calls keploy cannot capture.
func notCapturableReason(tests []string, what string) string {
	return fmt.Sprintf("not judged: %d test(s) ran while the application called what keploy cannot capture (%s): %s",
		len(tests), strings.Join(tests, ", "), strings.TrimPrefix(what, "not judged: "))
}

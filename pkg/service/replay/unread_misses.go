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

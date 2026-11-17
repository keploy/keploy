package replay

import (
	"fmt"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// A SET WHOSE RECORDING'S AGENT DROPPED MOCKS.
//
// The recording's agent can drop a mock after it captured it (its memory
// guard lets go of what was captured while it paused recording; a full or
// closed mock output loses what is sent to it), and keploy record marks the
// set's config with what the agent's summary said of them
// (models.TestSet.RecordMocksDropped). A mock no test case owned (one the
// application made as it started, one a session keeps for every test) costs
// no test case: the set's tests are kept, and a replay of them finds no mock
// for that call. Such a miss is the recording's, not the application's.
//
// So when a call of such a set found no mock (a test's own, one made before
// its first test or after its last, one a parser held), under the proven
// switch the set is not judged: its reason names the drop and the calls'
// tests, and says to record the set again. A set that stopped otherwise (its
// app exited, its user stopped it) keeps that status, and one not judged
// already names the drop in its reason too. With the switch off its verdict
// is as before, and a WARN names the drop beside a test's miss. A set none of
// whose calls missed is judged as any other: what was dropped, it did not
// need.

// mocksDroppedNext is what to do about a set whose recording's agent dropped
// mocks.
const mocksDroppedNext = "give the recording's keploy agent more memory (--memory-limit), or record with fewer concurrent requests, then record the test-set again"

// noteMissed keeps name in missed when res holds a call no mock answered
// that counts against a test (dataPlaneCalls), and takes it out otherwise: a
// test's last run decides, as a --retry-passing-test cycle's does.
func noteMissed(missed []string, name string, res *models.TestResult) []string {
	if res != nil && len(dataPlaneCalls(res.FailureInfo.UnmatchedCalls)) > 0 {
		return appendOnce(missed, name)
	}
	return slices.DeleteFunc(missed, func(n string) bool { return n == name })
}

// mocksDroppedWhy says why a set whose recording's agent dropped mocks d
// cannot vouch for a call that found no mock: in the tests missedTests, and
// setMisses calls no test owns.
func mocksDroppedWhy(d *models.MockDropSummary, missedTests []string, setMisses int) string {
	var where []string
	if len(missedTests) > 0 {
		where = append(where, fmt.Sprintf("%d test(s) (%s)", len(missedTests), strings.Join(missedTests, ", ")))
	}
	if setMisses > 0 {
		where = append(where, fmt.Sprintf("%d call(s) no test owns", setMisses))
	}
	why := fmt.Sprintf("a call of %s found no mock, and the recording's agent dropped mocks after capturing them (%d by memory pressure, %d for its mock output",
		strings.Join(where, " and "), d.ByPressure, d.Capacity)
	if len(d.Named) > 0 {
		why += "; first dropped: " + strings.Join(d.Named, ", ")
	}
	return why + "), so the mock it needed may be one the recording lacks"
}

package replay

import (
	"fmt"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
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
// is as before, and a WARN names the drop beside a test's miss and beside a
// call made outside every test that found no mock (outsideTestMisses: one
// made as the application started, or after the set's last test, as it
// stopped among them), and so, under the switch, does a set whose only such
// calls fail nothing (a DNS lookup, a background call): such a call may have
// needed a mock the recording lacks (sayMocksDroppedBesideMisses). A set none
// of whose calls missed is judged as any other: what was dropped, it did not
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

// sayMocksDroppedBesideMisses says, at WARN, with its next step, that the
// recording of the set testSetID lacks mocks its agent dropped (d), beside
// the calls of the set that found no mock and whose verdict says nothing of
// the drop. It is called once every read of the set is made, the set's end
// read after the application's stop among them, so the calls the
// application made outside every test (setCalls.outside) are all known.
// With the switch off those are each outside-test miss and the tests of
// missedTests, whose verdict is as before. Under the switch a set a test's
// call or a call no test owns (setFailing, the set's unclaimed calls) of
// which found no mock already named the drop with its verdict (RunTestSet),
// so nothing is said again; otherwise it is the outside-test misses, each of
// which fails nothing.
func (r *Replayer) sayMocksDroppedBesideMisses(d *models.MockDropSummary, testSetID string, missedTests []string, setFailing int) {
	if d == nil || !d.Dropped() {
		return
	}
	proven := provenswitch.On()
	if proven && (len(missedTests) > 0 || setFailing > 0) {
		return
	}
	outside := len(r.setCalls.outside(proven))
	if len(missedTests) == 0 && outside == 0 {
		return
	}
	r.logger.Warn(mocksDroppedWhy(d, missedTests, outside), zap.String("test-set", testSetID), zap.String("next_step", mocksDroppedNext))
}

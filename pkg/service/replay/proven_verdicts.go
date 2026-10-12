package replay

import (
	"fmt"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The per-test verdict of the proven model for `keploy test`, under the proven
// switch: a test fails when one of its calls got no recorded answer (a miss),
// or when it did not make a call it recorded (an unconsumed test mock). A test
// mock is a data-plane mock in the test's mapping (models.Mock.IsProtocolPhase
// says which are not), so eligibility is what the test recorded and not
// DeriveLifetime's tier, which leaves every untagged HTTP, SQL and Generic mock
// unchecked. With the switch off every function here keeps today's behaviour.

// assertTestMocks reports whether a test whose test mock went unconsumed is
// FAILED rather than demoted or passed: opted into with --assert-dependencies,
// and always under the proven switch.
func (r *Replayer) assertTestMocks() bool {
	return r.config.Test.AssertDependencies || provenswitch.On()
}

// excludedFromTestMockAssertion reports whether a loaded mock is left out of
// the per-test consumed-vs-expected assertion by name: a reusable-tier mock
// today, and only a protocol-phase mock under the proven switch.
func excludedFromTestMockAssertion(m *models.Mock) bool {
	if provenswitch.On() {
		return m.IsProtocolPhase()
	}
	return isReusableTierMock(m)
}

// unconsumedTestMocks is, under the proven switch, how many test mocks a test
// owns and those it did not consume, by name and kind, in recorded order. It is for
// the streaming pass; the main loop asks the same question through
// mockSetMismatch and buildDepResults. armed is whether the per-test mapping
// was in force on the agent: without it nothing is attributable to one test.
// With the switch off, or unarmed, a test owns nothing here.
func unconsumedTestMocks(armed bool, expected []models.MockEntry, consumed []models.MockState, kinds map[string]models.Kind, excluded map[string]bool) (int, []models.UnconsumedMock) {
	if !provenswitch.On() || !armed {
		return 0, nil
	}
	eligible := eligibleExpectedEntries(expected, kinds, excluded)
	got := make(map[string]bool, len(consumed))
	for _, m := range consumed {
		if !consumedOutsideAssertion(m) {
			got[m.Name] = true
		}
	}
	var missing []models.UnconsumedMock
	for _, m := range eligible {
		if !got[m.Name] {
			missing = append(missing, models.UnconsumedMock{Name: m.Name, Kind: m.Kind})
		}
	}
	return len(eligible), missing
}

// namesOf is the names of ms, in order.
func namesOf(ms []models.UnconsumedMock) []string {
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name
	}
	return names
}

// testMocksUnknown reports whether, under the proven switch, a test that owns
// test mocks has no verdict because what it consumed could not be read. Its
// test set is then not judged, rather than the test passing on evidence that
// was never seen.
func testMocksUnknown(armed bool, owned int, fetchErr error) bool {
	return provenswitch.On() && armed && owned > 0 && fetchErr != nil
}

// failTestsWithMisses fails, under the proven switch, every test that would
// otherwise pass or be demoted while one of its calls got no recorded answer,
// and names them. DNS is left out: resolution order is not deterministic, and
// a DNS miss is protocol-phase. Run over a set's results before the
// BeforeTestResult hook, so the hook sees the verdicts it is to act on.
func failTestsWithMisses(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var failed []string
	for i := range results {
		r := &results[i]
		if r.Status != models.TestStatusPassed && r.Status != models.TestStatusObsolete {
			continue
		}
		if dataPlaneMisses(r.FailureInfo.UnmatchedCalls) > 0 {
			r.Status = models.TestStatusFailed
			missCategory(r)
			failed = append(failed, r.TestCaseID)
		}
	}
	return failed
}

// dataPlaneMisses counts the calls that got no recorded answer, DNS lookups
// left out.
func dataPlaneMisses(calls []models.UnmatchedCall) int {
	n := 0
	for _, c := range calls {
		if !strings.EqualFold(c.Protocol, "DNS") {
			n++
		}
	}
	return n
}

// verdictCounts counts a set's per-test verdicts, one per test: the results the
// set persists, after the BeforeTestResult hook has set them. A test with more
// than one result counts once, by its least favourable one, so a duplicate row
// can never make a run greener.
func verdictCounts(results []models.TestResult) (passed, failed, obsolete, ignored int) {
	rank := map[models.TestStatus]int{
		models.TestStatusIgnored:  1,
		models.TestStatusPassed:   2,
		models.TestStatusObsolete: 3,
		models.TestStatusFailed:   4,
	}
	worst := make(map[string]models.TestStatus, len(results))
	var order []string
	for _, r := range results {
		prev, seen := worst[r.TestCaseID]
		if !seen {
			order = append(order, r.TestCaseID)
		}
		if !seen || rank[r.Status] > rank[prev] {
			worst[r.TestCaseID] = r.Status
		}
	}
	for _, id := range order {
		switch worst[id] {
		case models.TestStatusPassed:
			passed++
		case models.TestStatusFailed:
			failed++
		case models.TestStatusObsolete:
			obsolete++
		case models.TestStatusIgnored:
			ignored++
		}
	}
	return passed, failed, obsolete, ignored
}

// setTally is a test set's counts and status.
type setTally struct {
	success, failure, obsolete, ignored int
	status                              models.TestSetStatus
}

// tallyFromVerdicts is what the per-test verdicts, as the hook left them, make
// of a set under the proven switch: its counts and status from its result rows,
// when every test the loop scored has its row there to count (complete).
// Otherwise the loop's tally stands: a missing row cannot vouch for anything,
// and counting the rows that are there would pass a set whose failed test's
// row was lost.
func tallyFromVerdicts(loop setTally, results []models.TestResult, complete bool) setTally {
	if !complete {
		return loop
	}
	s, f, o, i := verdictCounts(results)
	return setTally{success: s, failure: f, obsolete: o, ignored: i, status: statusFromVerdicts(loop.status, f)}
}

// statusFromVerdicts is a set's status once its per-test verdicts decide it: a
// set the run judged (PASSED or FAILED) is FAILED when a test failed and PASSED
// otherwise; any other status (the app halted, keploy lost part of the run, the
// user stopped it) is kept, since no per-test verdict can vouch for the set.
func statusFromVerdicts(current models.TestSetStatus, failed int) models.TestSetStatus {
	if current != models.TestSetStatusPassed && current != models.TestSetStatusFailed {
		return current
	}
	if failed > 0 {
		return models.TestSetStatusFailed
	}
	return models.TestSetStatusPassed
}

// consumedUnreadReason is the test-set report's reason for a set left unjudged
// because the mocks some of its tests consumed could not be read. It names
// the first few; each one's row is marked (FailureInfo.TestMocksUnread).
func consumedUnreadReason(tests []string) string {
	return fmt.Sprintf("not judged: the mocks that %d test(s) consumed could not be read from keploy's agent (%s), so whether each made every recorded call is unknown; check the agent's log, then run the test-set again",
		len(tests), firstNames(tests, 10))
}

// firstNames joins up to n names, and says how many more there are.
func firstNames(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:n], ", "), len(names)-n)
}

// appendOnce appends name unless list holds it already (a test run again by
// a retry cycle is one test).
func appendOnce(list []string, name string) []string {
	if slices.Contains(list, name) {
		return list
	}
	return append(list, name)
}

// alsoNotJudged adds reason to why a set is not judged, keeping a reason
// already given.
func alsoNotJudged(prev, reason string) string {
	if prev == "" {
		return reason
	}
	return prev + "; " + reason
}

// vetoUnconsumedTestMock is the reason a streaming test whose mock set
// diverged is FAILED rather than demoted to OBSOLETE when one of its test
// mocks was not consumed (KEPLOY_PROVEN=1).
const vetoUnconsumedTestMock = "a recorded call of this test was not made during it (" + provenswitch.Env + "=1)"

// failOnMisses is the verdict of a test one of whose calls got no recorded
// answer, under the proven switch: FAILED, and its set with it, whatever the
// response did. A test already FAILED keeps the log line that says why.
func failOnMisses(o testOutcome) testOutcome {
	if o.Status == models.TestStatusFailed {
		return o
	}
	o.Status, o.FailsTestSet = models.TestStatusFailed, true
	if o.Log == mismatchLogIgnoredResponseMatched || o.Log == mismatchLogObsolete {
		// The mismatch line would say ignored or obsolete; the miss line says
		// why the test failed.
		o.Log = mismatchLogNone
	}
	return o
}

// missCategory adds UNMATCHED_CALL to a FAILED test whose call got no
// recorded answer (under the proven switch), so the report, JUnit and the
// NDJSON say why it failed.
func missCategory(r *models.TestResult) {
	if r == nil || r.Status != models.TestStatusFailed || dataPlaneMisses(r.FailureInfo.UnmatchedCalls) == 0 {
		return
	}
	r.FailureInfo.Category = appendCategoryUnique(slices.Clone(r.FailureInfo.Category), models.CallUnmatched)
}

// vetoUnmatchedCall is the reason a streaming test whose mock set diverged is
// FAILED rather than demoted to OBSOLETE when a call it made got no recorded
// answer (KEPLOY_PROVEN=1).
const vetoUnmatchedCall = "a call this test made got no recorded answer (" + provenswitch.Env + "=1)"

// holdEvidenceFloor sets back to FAILED, under the proven switch, every test a
// hook (BeforeTestResult) left PASSED or OBSOLETE while evidence stands
// against it: a call it made that no mock answered, or a recorded call it did
// not make. A hook may still quarantine such a test (IGNORED). It returns the
// tests it set back.
func holdEvidenceFloor(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var held []string
	for i := range results {
		r := &results[i]
		if r.Status != models.TestStatusPassed && r.Status != models.TestStatusObsolete {
			continue
		}
		if dataPlaneMisses(r.FailureInfo.UnmatchedCalls) == 0 && len(r.FailureInfo.UnconsumedMocks) == 0 {
			continue
		}
		r.Status = models.TestStatusFailed
		held = append(held, r.TestCaseID)
	}
	return held
}

// attachStreamDepResults writes a streamed test's dependency rows, under the
// proven switch: the same rows, category and unconsumed mocks a main-loop
// test gets (buildDepResults, attachDepResults), from the same inputs its
// verdict read (unconsumedTestMocks). Without the switch the streaming pass
// writes none (TestStreamingPathDoesNotWriteDepResults): its expected list was
// never filtered then, and is now, by eligibleExpectedEntries in both.
func attachStreamDepResults(result *models.TestResult, status models.TestStatus, expected []models.MockEntry, consumed []models.MockState, valid bool, excluded map[string]bool, kinds map[string]models.Kind, lookup map[string]mockDisplayInfo) {
	attachDepResults(result, status, buildDepResults(expected, consumed, valid, excluded, kinds, lookup), true)
}

// noteConsumers records that test consumed each of consumed.
func noteConsumers(consumers map[string][]string, test string, consumed []models.MockState) {
	for _, m := range consumed {
		if !slices.Contains(consumers[m.Name], test) {
			consumers[m.Name] = append(consumers[m.Name], test)
		}
	}
}

// markConsumedBy names, on each unconsumed test mock of the set's results
// (FailureInfo.UnconsumedMocks), the first other test that consumed it, if
// any: the call was made, by a sibling. It returns "test: mock (by sibling)"
// for each it named.
func markConsumedBy(results []models.TestResult, consumers map[string][]string) []string {
	var pairs []string
	for i := range results {
		r := &results[i]
		for j := range r.FailureInfo.UnconsumedMocks {
			u := &r.FailureInfo.UnconsumedMocks[j]
			for _, by := range consumers[u.Name] {
				if by != r.TestCaseID {
					u.ConsumedBy = by
					pairs = append(pairs, fmt.Sprintf("%s: %s (by %s)", r.TestCaseID, u.Name, by))
					break
				}
			}
		}
	}
	return pairs
}

// streamAssertedConsumption is what a streamed test consumed that its mock-set
// comparison reads. With the proven switch off it is all of it, as before.
// Under the switch the mocks the assertion does not own are left out of the
// consumed side as they are left out of the expected side: protocol-phase
// mocks by name (excluded, the set excludedFromTestMockAssertion builds, which
// covers MySQL's session-reusable commands and registered hooks) and the
// kinds and types consumedOutsideAssertion leaves out, so an extra COM_PING
// is no diverged mock set.
func streamAssertedConsumption(consumed []models.MockState, excluded map[string]bool) []models.MockState {
	if !provenswitch.On() {
		return consumed
	}
	out := make([]models.MockState, 0, len(consumed))
	for _, m := range consumed {
		if excluded[m.Name] || consumedOutsideAssertion(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

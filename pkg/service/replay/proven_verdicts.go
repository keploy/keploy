package replay

import (
	"fmt"
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
// owns and the names of those it did not consume, in recorded order. It is for
// the streaming pass; the main loop asks the same question through
// mockSetMismatch and buildDepResults. armed is whether the per-test mapping
// was in force on the agent: without it nothing is attributable to one test.
// With the switch off, or unarmed, a test owns nothing here.
func unconsumedTestMocks(armed bool, expected []models.MockEntry, consumed []models.MockState, kinds map[string]models.Kind, excluded map[string]bool) (int, []string) {
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
	var missing []string
	for _, m := range eligible {
		if !got[m.Name] {
			missing = append(missing, m.Name)
		}
	}
	return len(eligible), missing
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
// because the mocks some of its tests consumed could not be read.
func consumedUnreadReason(tests []string) string {
	return fmt.Sprintf("not judged: the mocks that %d test(s) consumed could not be read from keploy's agent (%s), so whether each made every recorded call is unknown; check the agent's log, then run the test-set again",
		len(tests), strings.Join(tests, ", "))
}

// alsoNotJudged adds reason to why a set is not judged, keeping a reason
// already given.
func alsoNotJudged(prev, reason string) string {
	if prev == "" {
		return reason
	}
	return prev + "; " + reason
}

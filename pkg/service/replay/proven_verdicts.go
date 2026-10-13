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
	// A status that is no verdict (PENDING, RUNNING, or anything a hook made
	// up) counts as failed: nothing vouches for the test.
	worst := make(map[string]models.TestStatus, len(results))
	var order []string
	for _, r := range results {
		prev, seen := worst[r.TestCaseID]
		if !seen {
			order = append(order, r.TestCaseID)
		}
		if !seen || statusRank(r.Status) > statusRank(prev) {
			worst[r.TestCaseID] = r.Status
		}
	}
	for _, id := range order {
		switch worst[id] {
		case models.TestStatusPassed:
			passed++
		case models.TestStatusObsolete:
			obsolete++
		case models.TestStatusIgnored:
			ignored++
		default:
			failed++
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
// A PASSED set none of whose tests has a verdict, every one IGNORED, is made
// IGNORED at the end of the set (everyTestIgnored), once every status that
// says more (not judged, a partial run) has had its say.
func statusFromVerdicts(current models.TestSetStatus, failed int) models.TestSetStatus {
	if current != models.TestSetStatusPassed && current != models.TestSetStatusFailed {
		return current
	}
	if failed > 0 {
		return models.TestSetStatusFailed
	}
	return models.TestSetStatusPassed
}

// everyTestIgnored reports whether, under the proven switch, a set about to be
// reported PASSED verified nothing: none of its tests passed, failed or was
// obsolete, and some were IGNORED, skipped one by one or quarantined by a hook.
// Such a set is reported IGNORED, as a set the run skips whole is, and never
// PASSED (design §8.2 rule 1: a run that verified nothing does not pass; the
// run's verdict is enterprise's, which fails such a run). A set with no test
// at all is scoredNothing's. With the switch off it stays PASSED.
func everyTestIgnored(status models.TestSetStatus, passed, failed, obsolete, ignored int) bool {
	return provenswitch.On() && status == models.TestSetStatusPassed &&
		passed+failed+obsolete == 0 && ignored > 0
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

// testEvidence is the evidence against a test, as its results carried it
// before the BeforeTestResult hook ran: the calls of it no mock answered
// (data-plane only), the recorded calls it did not make, and the categories
// that name them.
type testEvidence struct {
	misses     []models.UnmatchedCall
	unconsumed []models.UnconsumedMock
	categories []models.FailureCategory
}

// hookSnapshot is what the set's results said before the hook ran, under the
// proven switch: every test, in order, its least favourable row, and the
// evidence against it. Copied, so what the hook does to the results cannot
// reach it.
type hookSnapshot struct {
	tests    []string
	rows     map[string]models.TestResult
	evidence map[string]testEvidence
}

// dataPlaneCalls are the calls of calls that are not DNS lookups.
func dataPlaneCalls(calls []models.UnmatchedCall) []models.UnmatchedCall {
	var out []models.UnmatchedCall
	for _, c := range calls {
		if !strings.EqualFold(c.Protocol, "DNS") {
			out = append(out, c)
		}
	}
	return out
}

// statusRank orders verdicts from the most to the least favourable; a status
// that is no verdict ranks with FAILED.
func statusRank(s models.TestStatus) int {
	switch s {
	case models.TestStatusIgnored:
		return 1
	case models.TestStatusPassed:
		return 2
	case models.TestStatusObsolete:
		return 3
	default:
		return 4
	}
}

// cloneResult copies r with the slices the floor reads or restores, so the
// copy and r share nothing the hook could change.
func cloneResult(r models.TestResult) models.TestResult {
	r.FailureInfo.UnmatchedCalls = slices.Clone(r.FailureInfo.UnmatchedCalls)
	r.FailureInfo.UnconsumedMocks = slices.Clone(r.FailureInfo.UnconsumedMocks)
	r.FailureInfo.Category = slices.Clone(r.FailureInfo.Category)
	return r
}

// snapshotEvidence takes, under the proven switch, what the set's results say
// before the hook runs (nil with the switch off).
func snapshotEvidence(results []models.TestResult) *hookSnapshot {
	if !provenswitch.On() {
		return nil
	}
	snap := &hookSnapshot{rows: make(map[string]models.TestResult), evidence: make(map[string]testEvidence)}
	for _, r := range results {
		prev, seen := snap.rows[r.TestCaseID]
		if !seen {
			snap.tests = append(snap.tests, r.TestCaseID)
		}
		if !seen || statusRank(r.Status) > statusRank(prev.Status) {
			snap.rows[r.TestCaseID] = cloneResult(r)
		}
		misses := dataPlaneCalls(r.FailureInfo.UnmatchedCalls)
		if len(misses) == 0 && len(r.FailureInfo.UnconsumedMocks) == 0 {
			continue
		}
		e := snap.evidence[r.TestCaseID]
		e.misses = append(e.misses, misses...)
		e.unconsumed = append(e.unconsumed, r.FailureInfo.UnconsumedMocks...)
		for _, c := range r.FailureInfo.Category {
			if c == models.CallUnmatched || c == models.DependencyMissing {
				e.categories = appendCategoryUnique(e.categories, c)
			}
		}
		snap.evidence[r.TestCaseID] = e
	}
	return snap
}

// holdEvidenceFloor holds, under the proven switch, the set's verdicts against
// what the hook (BeforeTestResult) did to them; before is what they said
// before it ran (snapshotEvidence). It returns the results, with any it put
// back, and the tests it held. Each test is decided as a whole, over all its
// rows, because the tally counts a test by its least favourable row
// (verdictCounts):
//
//   - Every test keeps a row. A test the hook removed (or whose row it renamed
//     or overwrote) is put back, as its least favourable row was, FAILED, or
//     IGNORED if every row of it was IGNORED before the hook: a hook cannot
//     make a test vanish from the tally. A row of a test that did not exist
//     before the hook is FAILED.
//   - A test with evidence against it (a call it made that no mock answered,
//     or a recorded call it did not make, before the hook or still) counts
//     FAILED, unless every row of it is quarantined (IGNORED). When none of
//     its rows is FAILED, every row that is not IGNORED is set FAILED;
//     a test that has a FAILED row already counts FAILED, and its other rows
//     (a clean duplicate from an earlier cycle) are left as they are.
//   - A kind of evidence the hook cleared from every row of a test is put
//     back, with its category, on the row that carries the test's verdict.
func holdEvidenceFloor(results []models.TestResult, before *hookSnapshot) ([]models.TestResult, []string) {
	if !provenswitch.On() || before == nil {
		return results, nil
	}
	var held []string
	rowsOf := make(map[string][]int, len(results))
	for i, r := range results {
		rowsOf[r.TestCaseID] = append(rowsOf[r.TestCaseID], i)
	}
	for i := range results {
		if _, known := before.rows[results[i].TestCaseID]; !known && results[i].Status != models.TestStatusFailed {
			results[i].Status = models.TestStatusFailed
			held = appendOnce(held, results[i].TestCaseID)
		}
	}
	for _, id := range before.tests {
		if len(rowsOf[id]) > 0 {
			continue
		}
		row := cloneResult(before.rows[id])
		if row.Status != models.TestStatusIgnored {
			row.Status = models.TestStatusFailed
		}
		results = append(results, row)
		rowsOf[id] = []int{len(results) - 1}
		held = appendOnce(held, id)
	}
	for _, id := range before.tests {
		rows := rowsOf[id]
		snap := before.evidence[id]
		misses, unconsumed, allIgnored, failedRow := false, false, true, -1
		for _, i := range rows {
			misses = misses || dataPlaneMisses(results[i].FailureInfo.UnmatchedCalls) > 0
			unconsumed = unconsumed || len(results[i].FailureInfo.UnconsumedMocks) > 0
			allIgnored = allIgnored && results[i].Status == models.TestStatusIgnored
			if failedRow < 0 && results[i].Status == models.TestStatusFailed {
				failedRow = i
			}
		}
		if !misses && !unconsumed && len(snap.misses) == 0 && len(snap.unconsumed) == 0 {
			continue
		}
		// The row that carries the test's verdict, and so its evidence.
		verdictRow := rows[0]
		if failedRow >= 0 {
			verdictRow = failedRow
		} else if !allIgnored {
			for _, i := range rows {
				if results[i].Status != models.TestStatusIgnored {
					verdictRow = i
					break
				}
			}
		}
		v := &results[verdictRow]
		if !misses && len(snap.misses) > 0 {
			v.FailureInfo.UnmatchedCalls = append(slices.Clone(v.FailureInfo.UnmatchedCalls), snap.misses...)
		}
		if !unconsumed && len(snap.unconsumed) > 0 {
			v.FailureInfo.UnconsumedMocks = slices.Clone(snap.unconsumed)
		}
		if failedRow < 0 && !allIgnored {
			for _, i := range rows {
				if results[i].Status != models.TestStatusIgnored {
					results[i].Status = models.TestStatusFailed
				}
			}
			held = appendOnce(held, id)
		}
		for _, i := range rows {
			r := &results[i]
			if r.Status != models.TestStatusFailed && r.Status != models.TestStatusIgnored {
				continue
			}
			if i != verdictRow && dataPlaneMisses(r.FailureInfo.UnmatchedCalls) == 0 && len(r.FailureInfo.UnconsumedMocks) == 0 {
				continue
			}
			cats := slices.Clone(r.FailureInfo.Category)
			for _, c := range snap.categories {
				cats = appendCategoryUnique(cats, c)
			}
			r.FailureInfo.Category = cats
		}
	}
	return results, held
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

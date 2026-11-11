package replay

import (
	"context"
	"slices"
	"sort"
	"strings"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// The replay marks, as it reads a set, the recordings of calls the app made
// to its own port that it can tell from the recording alone
// (markOwnListenerMocks: a loopback destination at a test port). A recording
// of a call the app made to another of its addresses (its container's own IP)
// it cannot tell so. The agent can: as keploy test's proxy passes a call
// through to a listener of the app's own, it names that call's destination
// (models.OwnListenerCallsHeader), and the replay marks each recording made
// at one of those destinations too. It does so after every read of what a
// test consumed, before the test is judged, and after every read at the
// set's end, before the worker scope is judged. A recording at a destination
// the agent never named is judged as before.
//
// A test is judged when its own read is in, and the app may call its own
// listener after that read (an asynchronous health check, a call its answer
// set off). So a test whose judge charged it only recordings made at IP
// destinations the agent had not named yet is noted (ownListenerCharges). At
// the set's end, before anything reads its verdict, the replay asks the agent
// once more (ownListenerRefresher), marks what it names, and passes each
// noted test whose charge is now all marked (liftOwnListenerCharges): the
// agent passed those calls through to the app, so the test made no call of
// its recording from a mock.

// ownListenerReporter is the instrumentation that names the destinations
// keploy test's agent passed through as the app's own listener
// (platform/http.AgentClient.OwnListenerCalls). One that does not names none.
type ownListenerReporter interface {
	OwnListenerCalls() []string
}

// ownListenerRefresher is the instrumentation that can ask the agent again,
// without draining anything, which destinations it has passed through as the
// app's own listener (platform/http.AgentClient.ReadOwnListenerCalls), for
// OwnListenerCalls to name. One that cannot leaves what the last read of
// what the app consumed named.
type ownListenerRefresher interface {
	ReadOwnListenerCalls(ctx context.Context) error
}

// ownListenerReports is one set's recordings by where they were made, and the
// names of those marked as the app's calls to its own listener, at load or
// from the agent's reports. Its zero value marks nothing.
type ownListenerReports struct {
	// byDest: the names of the set's recordings by their destination, each
	// in models.OwnListenerDestination's spelling (the agent's).
	byDest map[string][]string
	// marked: the names of the recordings marked, read by every site that
	// charges a recording mid-set (exactPlan.ownListener).
	marked map[string]bool
	// indexed: the names byDest holds, which a report can still mark.
	indexed map[string]bool
}

// newOwnListenerReports indexes a set's recordings, from pools, by where they
// were made: a recording's destAddr, else an HTTP recording's Host, when it
// names an IP and a port. One marked at load is marked from the start.
func newOwnListenerReports(pools ...[]*models.Mock) ownListenerReports {
	o := ownListenerReports{byDest: map[string][]string{}, marked: map[string]bool{}, indexed: map[string]bool{}}
	for _, pool := range pools {
		for _, m := range pool {
			if m == nil {
				continue
			}
			if m.OwnListenerCall() {
				o.marked[m.Name] = true
				continue
			}
			addr := strings.TrimSpace(m.Spec.Metadata["destAddr"])
			if addr == "" {
				addr, _ = m.RecordedDestination()
			}
			if dest, ok := models.OwnListenerDestination(addr); ok {
				o.byDest[dest] = append(o.byDest[dest], m.Name)
				o.indexed[m.Name] = true
			}
		}
	}
	return o
}

// apply marks each recording made at one of dests (the agent's spelling) and
// not marked yet, and returns their names, sorted.
func (o ownListenerReports) apply(dests []string) []string {
	var names []string
	for _, dest := range dests {
		for _, name := range o.byDest[dest] {
			if !o.marked[name] {
				o.marked[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// applyOwnListenerReport marks, from what the agent's last answer named
// (ownListenerReporter), each of the set's recordings made at a destination
// the agent passed through as the app's own listener, and leaves it out of
// what a test requires: each map of excluded (the set's reusableMockNames and
// unservedMockNames) gets its name. The exact plan reads the same marks
// (exactPlan.ownListener) when it judges the worker scope and the shares. It
// says which it marked, and returns their names.
func (r *Replayer) applyOwnListenerReport(testSetID string, reports ownListenerReports, excluded ...map[string]bool) []string {
	var dests []string
	if rep, ok := r.instrumentation.(ownListenerReporter); ok {
		dests = rep.OwnListenerCalls()
	}
	names := reports.apply(dests)
	for _, m := range excluded {
		for _, name := range names {
			m[name] = true
		}
	}
	if len(names) > 0 {
		r.logger.Info("the application called its own listener at an address this test set's recordings hold calls to: keploy passed each such call through to the application, so these recordings go unused, and no test or worker scope requires them",
			zap.String("testSetID", testSetID), zap.Int("count", len(names)),
			zap.Strings("mocks", names[:min(len(names), maxNamedOwnListenerMocks)]))
	}
	return names
}

// refreshOwnListenerReport asks the agent, without draining anything, which
// destinations it has passed through as the app's own listener, when the
// instrumentation can (ownListenerRefresher). A failed read is said and
// leaves what the last read named: a test it could have passed keeps the
// verdict its judge gave (fail closed).
func (r *Replayer) refreshOwnListenerReport(ctx context.Context, testSetID string) {
	rf, ok := r.instrumentation.(ownListenerRefresher)
	if !ok {
		return
	}
	if err := rf.ReadOwnListenerCalls(ctx); err != nil {
		r.logger.Warn("keploy could not ask its agent, at the test set's end, which calls of the application to its own listener it passed through: a test charged a recording of such a call keeps the verdict its own read gave",
			zap.String("testSetID", testSetID), zap.Error(err))
	}
}

// ownListenerCharge is a test's judge's charge that the agent may yet name as
// the app's calls to its own listener: the recordings of the test's mapping
// it did not make (names), each made at an IP destination the agent had not
// named when the test was judged, while nothing else failed it. verdict is
// the row the judge wrote it in, which must still be the test's last
// recorded verdict at the set's end; consumed is what the test consumed;
// deps builds the row's dependency assertion again (buildDepResults), with
// the recordings marked by then left out.
type ownListenerCharge struct {
	names    []string
	consumed []models.MockState
	verdict  *models.TestResult
	deps     func() depAssertion
}

// ownListenerCharges is the set's charges by test name.
type ownListenerCharges map[string]ownListenerCharge

// note keeps, for test name, the charge its judge just gave in verdict:
// unconsumed are the recordings of its mapping it did not make, passed
// whether its verdict is PASSED without them. A test charged nothing, one
// that failed for anything else, or one charged a recording no report can
// mark (made at no IP destination, or marked already) has no charge: its
// verdict stands, and an earlier cycle's charge goes.
func (c ownListenerCharges) note(name string, reports ownListenerReports, passed bool, unconsumed []string, consumed []models.MockState, verdict *models.TestResult, deps func() depAssertion) {
	delete(c, name)
	if !passed || len(unconsumed) == 0 || verdict == nil {
		return
	}
	for _, n := range unconsumed {
		if !reports.indexed[n] || reports.marked[n] {
			return
		}
	}
	c[name] = ownListenerCharge{names: slices.Clone(unconsumed), consumed: consumed, verdict: verdict, deps: deps}
}

// recorded drops test name's charge when res, the verdict just recorded for
// it, is not the row the charge was noted in: a later cycle's verdict, or a
// row a terminal branch wrote, is judged as written.
func (c ownListenerCharges) recorded(name string, res *models.TestResult) {
	if ch, ok := c[name]; ok && ch.verdict != res {
		delete(c, name)
	}
}

// depsAgainOf builds a test's dependency assertion again, as its judge built
// it (buildDepResults, the same inputs), when the set's end asks: excluded is
// the set's reusableMockNames, which by then holds every recording marked as
// the app's own call, so those are left out.
func depsAgainOf(expected []models.MockEntry, consumed []models.MockState, valid bool, excluded map[string]bool, kinds map[string]models.Kind, lookup map[string]mockDisplayInfo) func() depAssertion {
	return func() depAssertion {
		return buildDepResults(expected, consumed, valid, excluded, kinds, lookup)
	}
}

// passesWithoutDependencyCharge is the verdict RunTestSet's chain gives a
// test (resolveTestOutcome, keepTemplateFailure, failOnMisses,
// failOnCaptureLoss) had it made every recorded call of its mapping: whether
// it is PASSED. Its answer, its templates, its misses (proven switch) and
// what keploy lost of its calls decide it, as they decide the test's own.
func passesWithoutDependencyCharge(answerMatched, templateFailed bool, earlyMissed int, lostCapture, noiseStrict, assertDependencies, strictFailure bool) bool {
	o := keepTemplateFailure(resolveTestOutcome(answerMatched, false, noiseStrict, assertDependencies, strictFailure), templateFailed)
	if earlyMissed > 0 {
		o = failOnMisses(o)
	}
	if lostCapture {
		o = failOnCaptureLoss(o)
	}
	return o.Status == models.TestStatusPassed
}

// unconsumedOf is the names of expected that consumed does not hold.
func unconsumedOf(expected, consumed []string) []string {
	var out []string
	for _, n := range expected {
		if !slices.Contains(consumed, n) {
			out = append(out, n)
		}
	}
	return out
}

// liftOwnListenerCharges passes, at the set's end, each charged test whose
// charge is now all marked as the app's calls to its own listener (marked):
// the agent passed those calls through, so the test made no call of its
// recording from a mock. Its last row, the verdict, if it still has the
// status its judge gave (a row is PASSED with the charge reported only, or
// FAILED by it), loses those recordings' evidence (unconsumed_mocks, the
// expected side of mock_mismatches, and its dep_result rows, built again
// without them), and a FAILED one
// loses DEPENDENCY_MISSING and is PASSED. It returns the tests it made
// PASSED and every test whose row it lifted, in the rows' order.
func liftOwnListenerCharges(results []models.TestResult, charges ownListenerCharges, marked map[string]bool) (passed, lifted []string) {
	last := map[string]int{}
	for i := range results {
		last[results[i].TestCaseID] = i
	}
	for i := range results {
		row := &results[i]
		ch, ok := charges[row.TestCaseID]
		if !ok || last[row.TestCaseID] != i || row.Status != ch.verdict.Status {
			continue
		}
		if row.Status != models.TestStatusPassed && row.Status != models.TestStatusFailed {
			continue
		}
		if slices.ContainsFunc(ch.names, func(n string) bool { return !marked[n] }) {
			continue
		}
		row.FailureInfo.UnconsumedMocks = slices.DeleteFunc(slices.Clone(row.FailureInfo.UnconsumedMocks), func(u models.UnconsumedMock) bool { return marked[u.Name] })
		if len(row.FailureInfo.UnconsumedMocks) == 0 {
			row.FailureInfo.UnconsumedMocks = nil
		}
		if ch.deps != nil {
			d := ch.deps()
			row.Result.DepResult, row.Result.DepsChecked, row.Result.DepsConsumed = d.Rows, d.Checked, d.Consumed
		}
		if mm := row.MockMismatches; mm != nil {
			row.MockMismatches = &models.MockMismatchInfo{
				ExpectedMocks: slices.DeleteFunc(slices.Clone(mm.ExpectedMocks), func(m models.MockMismatchMock) bool { return marked[m.Name] }),
				ActualMocks:   mm.ActualMocks,
			}
		}
		if row.Status == models.TestStatusFailed {
			row.Status = models.TestStatusPassed
			row.FailureInfo.Category = slices.DeleteFunc(slices.Clone(row.FailureInfo.Category), func(c models.FailureCategory) bool { return c == models.DependencyMissing })
			if len(row.FailureInfo.Category) == 0 {
				row.FailureInfo.Category = nil
			}
			passed = append(passed, row.TestCaseID)
		}
		lifted = append(lifted, row.TestCaseID)
	}
	return passed, lifted
}

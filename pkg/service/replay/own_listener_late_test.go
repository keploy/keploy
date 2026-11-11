package replay

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
)

const ownListenerLiftNote = "tests passed at the test set's end: every recorded call they did not make is a call the application made to its own listener"

// nthReadReporter is an agent that names dests from the at-th time the replay
// asks what its last read named (one ask follows each read of what the app
// consumed), and names nothing before.
type nthReadReporter struct {
	Instrumentation
	asked *atomic.Int32
	at    int32
	dests []string
}

func (i nthReadReporter) OwnListenerCalls() []string {
	if i.asked.Add(1) < i.at {
		return nil
	}
	return i.dests
}

// setEndReporter is an agent that names dests only once the set's end has
// asked it again (ReadOwnListenerCalls): the app called its own listener
// after the last read of what it consumed. fail makes that ask fail.
type setEndReporter struct {
	Instrumentation
	mu      *sync.Mutex
	refresh *int
	fail    error
	dests   []string
}

func (i setEndReporter) ReadOwnListenerCalls(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	*i.refresh++
	return i.fail
}

func (i setEndReporter) OwnListenerCalls() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if *i.refresh == 0 || i.fail != nil {
		return nil
	}
	return i.dests
}

// A test whose call to the app's own listener the agent names only after the
// test's own read is judged again at the set's end, before the set's verdict:
// named at the next test's read, or by the set's end's own ask, the
// recording goes unused and the test and its set pass, with no evidence of
// it left on the test's row; its live verdict is the final one, sent once.
// Never named (the ask fails, or names nothing), the test fails as before.
func TestARecordingTheAgentNamesAfterItsTestsReadIsNoCallTheSetsVerdictRequires(t *testing.T) {
	for _, mode := range []struct {
		name string
		on   bool
	}{{"switch on", true}, {"switch off, --assert-dependencies", false}} {
		for _, when := range []string{"at the next test's read", "at the set's end", "at the set's end, another test fails", "never: the ask fails", "never: nothing named", "nothing charged: it made the call"} {
			t.Run(mode.name+", "+when, func(t *testing.T) {
				setSwitch(t, mode.on)
				fastWorkerWait(t)
				tests := 1
				if when == "at the next test's read" || when == "at the set's end, another test fails" {
					tests = 2
				}
				h := newPartialRunHarness(t, tests, 0)
				pinWindows(h)
				port := appPortOf(t, h)
				h.replayer.config.Command = "./app"
				h.replayer.config.Test.AssertDependencies = !mode.on
				core, logs := observer.New(zap.InfoLevel)
				h.replayer.logger = zap.New(core)
				own := "172.18.0.5:" + port
				refresh := 0
				switch when {
				case "at the next test's read":
					h.replayer.instrumentation = nthReadReporter{Instrumentation: h.replayer.instrumentation, asked: &atomic.Int32{}, at: 2, dests: []string{own}}
				case "at the set's end", "at the set's end, another test fails", "nothing charged: it made the call":
					h.replayer.instrumentation = setEndReporter{Instrumentation: h.replayer.instrumentation, mu: &sync.Mutex{}, refresh: &refresh, dests: []string{own}}
				case "never: the ask fails":
					h.replayer.instrumentation = setEndReporter{Instrumentation: h.replayer.instrumentation, mu: &sync.Mutex{}, refresh: &refresh, fail: context.DeadlineExceeded, dests: []string{own}}
				default:
					h.replayer.instrumentation = setEndReporter{Instrumentation: h.replayer.instrumentation, mu: &sync.Mutex{}, refresh: &refresh}
				}
				h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), calledAt("mock-in-test", 1020, own)}
				mappingFile(t, h, map[string][]models.MockEntry{"test-1": {
					{Name: "mock-a", Kind: string(models.HTTP)},
					{Name: "mock-in-test", Kind: string(models.HTTP)},
				}})
				made := models.MockState{Name: "mock-a", Kind: models.HTTP, Usage: models.Deleted, Lifetime: models.LifetimePerTest,
					ReqTimestampMock: models.FormatMockTimestamp(attrAt(1010)), ResTimestampMock: models.FormatMockTimestamp(attrAt(1011))}
				consumed := []models.MockState{made}
				if when == "nothing charged: it made the call" {
					consumed = append(consumed, models.MockState{Name: "mock-in-test", Kind: models.HTTP, Usage: models.Deleted, Lifetime: models.LifetimePerTest,
						ReqTimestampMock: models.FormatMockTimestamp(attrAt(1020)), ResTimestampMock: models.FormatMockTimestamp(attrAt(1021))})
				}
				var reported []string
				h.replayer.hookImpl = prHooks{reported: &reported, consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": consumed}),
					wrongBody: map[string]bool{"test-2": when == "at the set's end, another test fails"}}
				set := h.run(t)
				got := resultOf(t, h, "test-1")
				var live []string
				for _, r := range reported {
					if r == "test-set-0/test-1="+string(models.TestStatusPassed) || r == "test-set-0/test-1="+string(models.TestStatusFailed) {
						live = append(live, r)
					}
				}
				if want := map[bool]int{true: 1, false: 0}[when != "at the next test's read" && when != "nothing charged: it made the call"]; refresh != want {
					t.Fatalf("the set's end asked the agent %d time(s); want %d (only when a test is charged)", refresh, want)
				}
				if want := map[bool]int{true: 1, false: 0}[when == "never: the ask fails"]; logs.FilterMessageSnippet("could not ask its agent, at the test set's end").Len() != want {
					t.Fatalf("the failed ask was said %d time(s); want %d", logs.FilterMessageSnippet("could not ask its agent, at the test set's end").Len(), want)
				}
				if when == "never: the ask fails" || when == "never: nothing named" {
					if set == models.TestSetStatusPassed || got.Status != models.TestStatusFailed {
						t.Fatalf("set %s, test-1 %s; want test-1 FAILED for mock-in-test, as before", set, got.Status)
					}
					if mode.on && (len(got.FailureInfo.UnconsumedMocks) != 1 || got.FailureInfo.UnconsumedMocks[0].Name != "mock-in-test") {
						t.Fatalf("test-1's unconsumed: %+v; want mock-in-test", got.FailureInfo.UnconsumedMocks)
					}
					if n := logs.FilterMessageSnippet(ownListenerLiftNote).Len(); n != 0 {
						t.Fatalf("a test was said to pass at the set's end: %d line(s)", n)
					}
					if !reflect.DeepEqual(live, []string{"test-set-0/test-1=" + string(models.TestStatusFailed)}) {
						t.Fatalf("test-1's live verdicts: %v; want FAILED, once", live)
					}
					return
				}
				if wantSet := map[bool]models.TestSetStatus{true: models.TestSetStatusFailed, false: models.TestSetStatusPassed}[when == "at the set's end, another test fails"]; set != wantSet {
					t.Fatalf("set %s, want %s (worker-scope call named %d time(s))", set, wantSet, workerNamed(logs))
				}
				if got.Status != models.TestStatusPassed || got.FailureInfo.UnconsumedMocks != nil || len(got.Result.DepResult) != 0 || got.FailureInfo.Category != nil {
					t.Fatalf("test-1: %s, unconsumed %+v, dep rows %+v, categories %v; want PASSED, no evidence", got.Status, got.FailureInfo.UnconsumedMocks, got.Result.DepResult, got.FailureInfo.Category)
				}
				if got.MockMismatches != nil && when != "nothing charged: it made the call" {
					for _, m := range got.MockMismatches.ExpectedMocks {
						if m.Name == "mock-in-test" {
							t.Fatalf("test-1's report still expects the self-call: %+v", got.MockMismatches.ExpectedMocks)
						}
					}
				}
				if want := map[bool]int{true: 0, false: 1}[when == "nothing charged: it made the call"]; logs.FilterMessageSnippet(ownListenerLiftNote).Len() != want {
					t.Fatalf("the set's end said %d line(s) of tests passed; want %d", logs.FilterMessageSnippet(ownListenerLiftNote).Len(), want)
				}
				if !reflect.DeepEqual(live, []string{"test-set-0/test-1=" + string(models.TestStatusPassed)}) {
					t.Fatalf("test-1's live verdicts: %v; want PASSED, once", live)
				}
			})
		}
	}
}

// A charge is noted only when the test's verdict is PASSED without it and
// every recording of it can still be named by the agent (made at an IP
// destination, not marked); a later verdict of the test drops it.
func TestOwnListenerChargesNoteOnlyWhatTheAgentCanStillLift(t *testing.T) {
	reports := newOwnListenerReports([]*models.Mock{calledAt("own", 1, "172.18.0.5:6100"), httpAt("nowhere", 2), calledAt("named", 3, "172.18.0.5:6101")})
	reports.apply([]string{"172.18.0.5:6101"})
	row := &models.TestResult{TestCaseID: "t", Status: models.TestStatusFailed}
	for _, c := range []struct {
		name       string
		passed     bool
		unconsumed []string
		verdict    *models.TestResult
		want       bool
	}{
		{"made at an IP destination", true, []string{"own"}, row, true},
		{"failed for something else too", false, []string{"own"}, row, false},
		{"charged nothing", true, nil, row, false},
		{"one made at no IP destination", true, []string{"own", "nowhere"}, row, false},
		{"one marked already", true, []string{"own", "named"}, row, false},
		{"no row", true, []string{"own"}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			charges := ownListenerCharges{"t": {names: []string{"stale"}, verdict: &models.TestResult{}}}
			charges.note("t", reports, c.passed, c.unconsumed, nil, c.verdict, nil)
			if _, ok := charges["t"]; ok != c.want {
				t.Fatalf("noted %v, want %v (an earlier charge never stands)", ok, c.want)
			}
		})
	}
	charges := ownListenerCharges{}
	charges.note("t", reports, true, []string{"own"}, nil, row, nil)
	charges.recorded("t", row)
	if _, ok := charges["t"]; !ok {
		t.Fatal("recording the row the charge was noted in dropped it")
	}
	charges.recorded("t", &models.TestResult{TestCaseID: "t", Status: models.TestStatusFailed})
	if _, ok := charges["t"]; ok {
		t.Fatal("a later verdict of the test kept the charge")
	}
}

// The chain without the charge: an answer that differs, a template that
// failed it, a miss or a lost call each keep the verdict from PASSED.
func TestPassesWithoutDependencyChargeFollowsTheVerdictsChain(t *testing.T) {
	for _, c := range []struct {
		name     string
		matched  bool
		template bool
		missed   int
		lost     bool
		want     bool
	}{
		{"answer matched", true, false, 0, false, true},
		{"answer differs", false, false, 0, false, false},
		{"answer differs, a template failed", false, true, 0, false, false},
		{"a call got no recorded answer", true, false, 1, false, false},
		{"a call was lost", true, false, 0, true, false},
	} {
		for _, flags := range [][3]bool{{false, false, false}, {true, true, true}} {
			if got := passesWithoutDependencyCharge(c.matched, c.template, c.missed, c.lost, flags[0], flags[1], flags[2]); got != c.want {
				t.Fatalf("%s, flags %v: %v, want %v", c.name, flags, got, c.want)
			}
		}
	}
}

// The set's end lifts a test's last row only when its charge is all marked
// and the row still has the status its judge gave; a PASSED row loses only
// the evidence, a FAILED one is PASSED too.
func TestLiftOwnListenerChargesPassesOnlyAWhollyMarkedVerdict(t *testing.T) {
	failedRow := func(id string, names ...string) models.TestResult {
		r := models.TestResult{TestCaseID: id, Status: models.TestStatusFailed}
		r.FailureInfo.Category = []models.FailureCategory{models.DependencyMissing}
		r.MockMismatches = &models.MockMismatchInfo{ExpectedMocks: []models.MockMismatchMock{{Name: "kept"}}}
		for _, n := range names {
			r.FailureInfo.UnconsumedMocks = append(r.FailureInfo.UnconsumedMocks, models.UnconsumedMock{Name: n})
			r.Result.DepResult = append(r.Result.DepResult, models.DepResult{Name: n})
			r.MockMismatches.ExpectedMocks = append(r.MockMismatches.ExpectedMocks, models.MockMismatchMock{Name: n})
		}
		return r
	}
	results := []models.TestResult{
		{TestCaseID: "earlier-cycle", Status: models.TestStatusPassed},
		failedRow("all-marked", "own-1", "own-2"),
		failedRow("half-marked", "own-1", "other"),
		failedRow("hook-changed", "own-1"),
		{TestCaseID: "earlier-cycle", Status: models.TestStatusFailed},
		{TestCaseID: "passed-reported", Status: models.TestStatusPassed, Result: models.Result{DepResult: []models.DepResult{{Name: "own-1"}}}},
		{TestCaseID: "obsolete", Status: models.TestStatusObsolete},
	}
	results[3].Status = models.TestStatusIgnored
	charges := ownListenerCharges{}
	for _, c := range []struct {
		id    string
		names []string
		was   models.TestStatus
	}{
		{"all-marked", []string{"own-1", "own-2"}, models.TestStatusFailed},
		{"half-marked", []string{"own-1", "other"}, models.TestStatusFailed},
		{"hook-changed", []string{"own-1"}, models.TestStatusFailed},
		{"earlier-cycle", []string{"own-1"}, models.TestStatusPassed},
		{"passed-reported", []string{"own-1"}, models.TestStatusPassed},
		{"obsolete", []string{"own-1"}, models.TestStatusObsolete},
	} {
		charges[c.id] = ownListenerCharge{names: c.names, verdict: &models.TestResult{Status: c.was},
			deps: func() depAssertion { return depAssertion{Checked: true, Consumed: 1} }}
	}
	marked := map[string]bool{"own-1": true, "own-2": true}
	passed, lifted := liftOwnListenerCharges(results, charges, marked)
	if !reflect.DeepEqual(passed, []string{"all-marked"}) || !reflect.DeepEqual(lifted, []string{"all-marked", "passed-reported"}) {
		t.Fatalf("passed %v, lifted %v; want [all-marked], [all-marked passed-reported]", passed, lifted)
	}
	all := results[1]
	if all.Status != models.TestStatusPassed || all.FailureInfo.UnconsumedMocks != nil || len(all.Result.DepResult) != 0 || !all.Result.DepsChecked || all.Result.DepsConsumed != 1 || all.FailureInfo.Category != nil {
		t.Fatalf("all-marked: %+v", all)
	}
	if got := all.MockMismatches.ExpectedMocks; len(got) != 1 || got[0].Name != "kept" {
		t.Fatalf("all-marked expects %+v; want only the recording it made", got)
	}
	if half := results[2]; half.Status != models.TestStatusFailed || len(half.FailureInfo.UnconsumedMocks) != 2 || len(half.Result.DepResult) != 2 {
		t.Fatalf("half-marked was lifted: %+v", half)
	}
	if results[3].Status != models.TestStatusIgnored || results[4].Status != models.TestStatusFailed || results[6].Status != models.TestStatusObsolete {
		t.Fatalf("a row whose status is not its judge's was lifted: %s %s %s", results[3].Status, results[4].Status, results[6].Status)
	}
	if len(results[5].Result.DepResult) != 0 || results[5].Status != models.TestStatusPassed {
		t.Fatalf("passed-reported keeps %+v", results[5])
	}
	if _, l := liftOwnListenerCharges([]models.TestResult{failedRow("all-marked", "own-1")}, charges, nil); l != nil {
		t.Fatalf("lifted %v with nothing marked", l)
	}
}

package replay

import (
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A call the application makes between two tests, after the agent read the
// earlier one's misses, is its background work (founder D1): the agent hands
// it to the next test's read marked Background, and keploy test charges it to
// no test, fails nothing with it and lists it once for the set, as keploy
// mock does. It was charged to the next test, so a test that followed a
// quarantined one failed for the timer and cache-fill calls made while the
// quarantined one was skipped (api-server's TestNonExistentApp).

// betweenTests is a call made between two tests that no mock answered.
func betweenTests(summary string) models.UnmatchedCall {
	return models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: summary, Destination: "mongo:27017",
		At: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), Kind: models.MissOutsideTest, Background: true,
		MatchPhase: "no_mock_for_destination", ClosestMock: "mock-12"}
}

// setBackgroundRecorder is the run's hooks with SetBackgroundHook.
type setBackgroundRecorder struct {
	TestHooks
	mu   sync.Mutex
	sets []string
	got  [][]models.UnmatchedCall
}

func (h *setBackgroundRecorder) SetBackground(testSetID string, calls []models.UnmatchedCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sets = append(h.sets, testSetID)
	h.got = append(h.got, slices.Clone(calls))
}

// TestNonExistentApp's shape: the test before it was set aside, and the
// application's timer and the set-aside test's cache fill missed before it
// began. It passes, the set passes, and both calls are listed for the set and
// said once, at WARN, with when and why.
func TestRunTestSet_CallsMadeBetweenTwoTestsFailNeitherNorTheSet(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	core, logs := observer.New(zap.WarnLevel)
	h.replayer.logger = zap.New(core)
	timer := betweenTests("update ordinals where dirtyAt <= now")
	fill := betweenTests("find vault_pats")
	h.instr.mockErrorsFor = misses(nil, reads(timer, fill))

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); a call made between two tests fails no set", status, h.report.report.FailureReason)
	}
	rep := h.report.report
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed || len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("test %s is %s with %+v; a call made between two tests is no test's", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls)
		}
	}
	if len(rep.UnclaimedCalls) != 0 {
		t.Fatalf("unclaimed calls %+v; a background call fails nothing", rep.UnclaimedCalls)
	}
	if len(rep.BackgroundCalls) != 2 || rep.BackgroundCalls[0].ActualSummary != timer.ActualSummary || rep.BackgroundCalls[1].ActualSummary != fill.ActualSummary {
		t.Fatalf("background calls %+v; want the timer's and the cache fill, in read order", rep.BackgroundCalls)
	}
	said := logs.FilterMessageSnippet("outside every test had no recorded answer").All()
	if len(said) != 2 {
		t.Fatalf("%d WARN line(s) for the background calls, want one each: %+v", len(said), logs.All())
	}
	fields := said[0].ContextMap()
	if fields["call"] != timer.ActualSummary || fields["destination"] != "mongo:27017" || fields["test-set"] != "test-set-0" ||
		fields["why"] != "the match stopped at no_mock_for_destination; closest mock-12" {
		t.Fatalf("the WARN line does not say what was called, where, and why: %+v", fields)
	}
	if _, ok := fields["at"]; !ok {
		t.Fatalf("the WARN line does not say when the call was made: %+v", fields)
	}
}

// A test's own miss beside a background call still fails that test, naming
// only its own call; the background call stays the set's listing.
func TestRunTestSet_ATestsOwnMissBesideABackgroundCallStillFailsIt(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	own := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /own", Destination: "deps:80", Kind: models.MissInTest}
	h.instr.mockErrorsFor = misses(nil, reads(betweenTests("find vault_pats"), own))

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s; the second test's own miss fails it", status)
	}
	rep := h.report.report
	var failed []models.TestResult
	for _, r := range rep.Tests {
		if r.Status == models.TestStatusFailed {
			failed = append(failed, r)
		}
	}
	if len(failed) != 1 || len(failed[0].FailureInfo.UnmatchedCalls) != 1 || failed[0].FailureInfo.UnmatchedCalls[0].ActualSummary != "GET /own" {
		t.Fatalf("failed tests %+v; want the second, naming only its own call", failed)
	}
	if len(rep.BackgroundCalls) != 1 || rep.BackgroundCalls[0].ActualSummary != "find vault_pats" {
		t.Fatalf("background calls %+v; want the cache fill", rep.BackgroundCalls)
	}
}

// The set keeps its background calls apart from its own and the test's, and
// a set run again starts with none.
func TestBackgroundCallsAreTheSetsListingOnly(t *testing.T) {
	var s setCalls
	before, _ := beforeFirst()
	own := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /own"}
	bg := betweenTests("find vault_pats")
	rest := s.take(append(before, bg, own))
	if len(rest) != 1 || rest[0].ActualSummary != "GET /own" {
		t.Fatalf("the test keeps %+v; want only its own call", rest)
	}
	if got := s.failing(); len(got) != 1 || !got[0].BeforeSet {
		t.Fatalf("the set fails with %+v; want only the call made before it", got)
	}
	if got := s.backgroundCalls(); len(got) != 1 || got[0].ActualSummary != bg.ActualSummary {
		t.Fatalf("the set lists %+v; want the background call", got)
	}
	s.reset()
	if got := s.backgroundCalls(); len(got) != 0 {
		t.Fatalf("after the reset the set lists %+v; want nothing", got)
	}
}

// The set's background calls reach SetBackgroundHook as the set's end is told,
// under the switch only, and not for an end its report does not say.
func TestBackgroundCallsAreToldToTheHooks(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	rec := &setBackgroundRecorder{TestHooks: h.replayer.GetTestHooks()}
	h.replayer.SetTestHooks(rec)
	h.instr.mockErrorsFor = misses(nil, reads(betweenTests("find vault_pats")))

	status := h.run(t)
	h.replayer.tellSetEnd("test-set-0", status, nil)
	if len(rec.sets) != 1 || rec.sets[0] != "test-set-0" || len(rec.got[0]) != 1 || rec.got[0][0].ClosestMock != "mock-12" {
		t.Fatalf("SetBackgroundHook was told %v %+v; want test-set-0's background call, once", rec.sets, rec.got)
	}
	h.replayer.tellSetEnd("test-set-0", models.TestSetStatusInternalErr, nil)
	if len(rec.got) != 2 || rec.got[1] != nil {
		t.Fatalf("an end its report does not say told %+v; want none", rec.got)
	}
	provenOn(t, false)
	h.replayer.tellSetEnd("test-set-0", status, nil)
	if len(rec.sets) != 2 {
		t.Fatalf("SetBackgroundHook was told %d times; without the switch it is not told", len(rec.sets))
	}
}

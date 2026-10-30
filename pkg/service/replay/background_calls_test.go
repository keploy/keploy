package replay

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A call the application makes between two tests, after the agent read the
// earlier one's misses, is its background work: the agent hands
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
	rest := s.take(context.Background(), append(before, bg, own), "test-2", nil)
	if len(rest) != 1 || rest[0].ActualSummary != "GET /own" {
		t.Fatalf("the test keeps %+v; want only its own call", rest)
	}
	if got := s.backgroundCalls(); len(got) != 2 || !got[0].BeforeSet || got[1].ActualSummary != bg.ActualSummary {
		t.Fatalf("the set lists %+v; want the call made before it, then the background call", got)
	}
	s.reset(nil)
	if got := s.backgroundCalls(); len(got) != 0 {
		t.Fatalf("after the reset the set lists %+v; want nothing", got)
	}
}

// inTestSeries is a call of a series judging marked as the application's
// background work, made inside a test's window, that no recording of the
// series answered: its parser marks it (keploy enterprise's series answerer).
func inTestSeries(summary string) models.UnmatchedCall {
	c := betweenTests(summary)
	c.Kind = models.MissInTest
	return c
}

// A marked background series' call is background work wherever it was made,
// and the set's line says where: made inside test-1's window it is no call
// made outside every test (where keploy once said
// "outside every test" of a series' miss inside a GET's window), and one the
// set end's read takes was made inside the last test's window.
func TestRunTestSet_AMarkedSeriesCallIsSaidAsMadeInsideItsTestsWindow(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	core, logs := observer.New(zap.WarnLevel)
	h.replayer.logger = zap.New(core)
	inFirst, late := inTestSeries("findAndModify on jobs"), inTestSeries("update jobs")
	h.instr.mockErrorsFor = misses(reads(inFirst), nil, reads(late))

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); a marked series' call fails no set", status, h.report.report.FailureReason)
	}
	rep := h.report.report
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed || len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("test %s is %s with %+v; a marked series' call is no test's", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls)
		}
	}
	// The set stores no mock-12: keploy cannot tell whether its answer
	// reaches test-1, and says so.
	want := []string{madeInside("test-1") + ", and keploy could not tell whether its recorded answer reaches a test: its closest recording mock-12 is not in the set's stored recordings", madeInside("test-2")}
	if len(rep.BackgroundCalls) != 2 || rep.BackgroundCalls[0].BackgroundWhy != want[0] || rep.BackgroundCalls[1].BackgroundWhy != want[1] {
		t.Fatalf("background calls %+v; want each said as made inside its test's window: %q", rep.BackgroundCalls, want)
	}
	if out := logs.FilterMessageSnippet("outside every test").All(); len(out) != 0 {
		t.Fatalf("%d line(s) say a call made inside a test's window was made outside every test: %+v", len(out), out)
	}
	said := logs.FilterMessageSnippet("a call of a marked background series, made inside a test's window, had no recorded answer").All()
	if len(said) != 2 {
		t.Fatalf("%d WARN line(s) for the marked series' calls, want one each: %+v", len(said), logs.All())
	}
	for i, l := range said {
		if l.ContextMap()["where"] != want[i] {
			t.Fatalf("line %d says where %q; want %q", i, l.ContextMap()["where"], want[i])
		}
	}
}

package learn

import (
	"strings"
	"testing"
	"time"
)

// trailSent is the place test-1's own call left the run (Earlier.Run): its
// request sent id r in the recording and f in round F, the id test-1
// minted. The call decided is test-2's, which carries it.
func trailSent(r, f string) []Earlier {
	sent := oSent(r, f)
	sent[0].Own, sent[0].Run = false, true
	return sent
}

func trailInput(r, live string, trail Trail) Input {
	in := oInput("body.user", r, live, trailSent(r, "Zq81Lm0XpVb4"), nil)
	in.Trails = trail
	return in
}

// A call keploy record marked trailing test-1 (it carries the id test-1's
// own call sent, and was recorded after test-1 ended) reads the place
// test-1's call left the run as its own: the id is bound there, as the
// run's variable (From.Run), which a replay keeps past test-1's end, and
// the line names the test the call trails.
func TestATrailingCallBindsTheIDItsTestSentAsTheRunsVariable(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	d := oWord(t, Decide(trailInput(r, f, Trail{Test: "test-1", Mocks: []string{"mock-9", "mock-0"}})), r, f)
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-0" || d.From.Path != "body.user" || !d.From.Run || d.From.InStartUp {
		t.Fatalf("the trailing call's id: %s, from %+v; want a value bound at mock-0 body.user as the run's variable", oDescribe([]Decision{d}), d.From)
	}
	if !strings.Contains(d.Line, "sent in test-1, which this call trails") {
		t.Errorf("the line %q does not name the test the call trails", d.Line)
	}
}

// Every other call stays refused as main's flip rules a value another test
// sent: with no trail, with a trail that names no test, and with a trail of
// another test (the place is not one of its recordings). The refusal names
// test-1's place and the mappings edits.
func TestAValueAnotherTestSentStaysRefusedUnlessTheCallTrailsThatTest(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	for _, tc := range []struct {
		name  string
		trail Trail
	}{
		{"no trail", Trail{}},
		{"a trail that names no test", Trail{Mocks: []string{"mock-0"}}},
		{"a trail of another test", Trail{Test: "test-0", Mocks: []string{"mock-7"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := oWord(t, Decide(trailInput(r, f, tc.trail)), r, f)
			oRefused(t, tc.name, d, RefusalAnotherScope, "mock-0", r)
			if !oEdits(d.Refusal, EditMoveToMocks, EditMoveToStartup, EditNoiseAtField) {
				t.Errorf("no mappings edit to act on: %+v", d.Refusal)
			}
			if strings.Contains(d.Line, "trails") {
				t.Errorf("the line %q names a trail the call does not have", d.Line)
			}
		})
	}
}

// A trailing call that carries another id than the one its test sent in
// round F follows nothing of the test's: the place sent the recorded id
// whole and round F's other value did not follow, so the word is refused,
// never bound or learned fresh.
func TestATrailingCallCarryingAnotherIDIsRefused(t *testing.T) {
	const r, other = "rAHcEraS7Qx1", "Pk20Wd7YcRt5"
	d := oWord(t, Decide(trailInput(r, other, Trail{Test: "test-1", Mocks: []string{"mock-0"}})), r, other)
	if d.Outcome != OutcomeRefused {
		t.Fatalf("a trailing call with another id: %s; want refused", oDescribe([]Decision{d}))
	}
}

// Only a place an earlier test's call left the run is the trailed test's:
// an exchange the call's own test was served (Own, no Run) is bound as the
// test's own variable, trailed or not, and a start-up's place as the
// start-up's.
func TestATrailReadsOnlyTheRunsPlacesAsTheTrailedTests(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	trail := Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	own := oInput("body.user", r, f, oSent(r, f), nil)
	own.Trails = trail
	if d := oWord(t, Decide(own), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("the test's own exchange: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	startUp := oSent(r, f)
	startUp[0].Own, startUp[0].Run, startUp[0].StartUp, startUp[0].At = false, true, true, oAt.Add(-time.Minute)
	in := oInput("body.user", r, f, startUp, nil)
	in.Trails = trail
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || !d.From.InStartUp || strings.Contains(d.Line, "trails") {
		t.Fatalf("the start-up's exchange: %s, from %+v; want the start-up's variable", oDescribe([]Decision{d}), d.From)
	}
}

// Where mappings.yaml places the exchange (Input.Scope), its list decides:
// one it lists in the call's own test is the test's own variable, trailed
// or not; one it lists in the trailed test's that the call's own test was
// served (no place an earlier test's call left the run) stays refused, as
// a recording of another test round F served the call's test.
func TestATrailReadsMappingsPlaceOfTheExchange(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	trail := Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	scope := func(s Scope) func(string) Scope { return func(string) Scope { return s } }
	in := trailInput(r, f, trail)
	in.Scope = scope(ScopeTest("test-2"))
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("listed in the call's own test: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	in = oInput("body.user", r, f, oSent(r, f), nil)
	in.Trails, in.Scope = trail, scope(ScopeTest("test-1"))
	oRefused(t, "the trailed test's recording served in the call's test", oWord(t, Decide(in), r, f), RefusalAnotherScope, "mock-0", r)
}

// A trailing call the round made inside the trailed test's own window (the
// application ran its clean-up before the test's last answer, sooner than
// it did at record) reads the place the test was served there as its own,
// as every call of the test does, and binds the id there as the run's
// variable, never the test's own: a replay where the call lands after the
// test ended binds it as well. The test's own place of a recording its
// list does not hold stays the test's own variable, and a trailing call
// landing in another test's window gets the run's variable only from a
// place the trailed test left the run (TestATrailReadsOnlyTheRunsPlacesAsTheTrailedTests).
func TestATrailingCallInItsTestsOwnWindowBindsTheRunsVariable(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	in := oInput("body.user", r, f, oSent(r, f), nil)
	in.Test, in.Trails = "test-1", Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	d := oWord(t, Decide(in), r, f)
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-0" || !d.From.Run || d.From.InStartUp || !strings.Contains(d.Line, "sent in test-1, which this call trails") {
		t.Fatalf("the trailing call in test-1's own window: %s, from %+v; want a value bound at mock-0 as the run's variable, naming the trail", oDescribe([]Decision{d}), d.From)
	}
	in.Trails = Trail{Test: "test-1", Mocks: []string{"mock-7"}}
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("a place the trailed test's list does not hold: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	in.Trails = Trail{}
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("no trail: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	// A start-up's place stays the start-up's, and one mappings.yaml lists
	// in another test stays refused as that test's, in the trailed test's
	// own window too.
	trail := Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	startUp := oSent(r, f)
	startUp[0].Own, startUp[0].Run, startUp[0].StartUp, startUp[0].At = false, true, true, oAt.Add(-time.Minute)
	in = oInput("body.user", r, f, startUp, nil)
	in.Test, in.Trails = "test-1", trail
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || !d.From.InStartUp || strings.Contains(d.Line, "trails") {
		t.Fatalf("the start-up's exchange in test-1's window: %s, from %+v; want the start-up's variable, naming no trail", oDescribe([]Decision{d}), d.From)
	}
	in = oInput("body.user", r, f, oSent(r, f), nil)
	in.Test, in.Trails = "test-1", trail
	in.Scope = func(string) Scope { return ScopeTest("test-0") }
	oRefused(t, "an exchange mappings.yaml lists in test-0, in test-1's window", oWord(t, Decide(in), r, f), RefusalAnotherScope, "mock-0", r)
}

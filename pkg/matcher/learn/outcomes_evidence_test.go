package learn

import (
	"strings"
	"testing"
	"time"
)

// Rows for the evidence at a place (Decide): which earlier places count,
// each run's state there, and what a place that could not be read does to
// every change of the call.

// A word of a path ("uploads/17/a.png": '/' and '.' always split) is judged
// by its places like any word: bound to the call's own test's answer that
// gave it, refused naming the edit where only another test's did, and made
// anew each run where no place holds it.
func TestOutcomeAWordOfAPathIsJudgedByItsPlaces(t *testing.T) {
	const p = "body.key"
	gave := func(run bool) []Earlier {
		return oServed("mock-20", run, false, Response, oText("body.id", "17"), "18", time.Millisecond)
	}
	d := oWord(t, Decide(oInput(p, "uploads/17/a.png", "uploads/18/a.png", gave(false), nil)), "17", "18")
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-20" {
		t.Errorf("uploads/17 beside the own test's id 17 to 18: %s; want a Value from mock-20", oDescribe([]Decision{d}))
	}
	d = oWord(t, Decide(oInput(p, "uploads/17/a.png", "uploads/18/a.png", gave(true), nil)), "17", "18")
	oRefused(t, "uploads/17 beside another test's id 17 to 18", d, "another-scope", "mock-20")
	d = oWord(t, Decide(oInput(p, "uploads/17/a.png", "uploads/18/a.png", nil, nil)), "17", "18")
	if d.Outcome != OutcomeFresh {
		t.Errorf("uploads/17 with no place: %s; want fresh", oDescribe([]Decision{d}))
	}
}

// A source served between tests (no test open, after the start-up) is in no
// scope the call binds in: refused, naming the place and the mappings edit
// that would let it bind (move it to the test's mocks: or to startup:), or
// noise at the word. keploy never moves a mock itself.
func TestOutcomeASourceBetweenTestsIsRefusedNamingTheEdit(t *testing.T) {
	between := oServed("mock-21", true, false, Response, oText("body.token", "tok-9a1"), "tok-9b2", time.Millisecond)
	d := oWord(t, Decide(oInput("header.Authorization", "Bearer tok-9a1", "Bearer tok-9b2", between, nil)), "9a1", "9b2")
	oRefused(t, "a token served between tests", d, "another-scope", "mock-21")
	if !oEdits(d.Refusal, "move-to-mocks", "move-to-startup", "noise-at-word") {
		t.Errorf("a source between tests: edits %+v; want move to mocks:, move to startup:, noise at the word", d.Refusal)
	}
	// The same served at the start-up binds.
	startUp := oServed("mock-21", true, true, Response, oText("body.token", "tok-9a1"), "tok-9b2", time.Millisecond)
	d = oWord(t, Decide(oInput("header.Authorization", "Bearer tok-9a1", "Bearer tok-9b2", startUp, nil)), "9a1", "9b2")
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-21" {
		t.Errorf("a token served at the start-up: %s; want a Value from mock-21", oDescribe([]Decision{d}))
	}
}

// mappings.yaml names each mock's scope, and that scope decides, not where
// the mock was recorded. A source mappings.yaml lists under worker: is out
// of the call's scope even where it was recorded in this test's window:
// refused, naming worker: and the edit (worker: is served, never required,
// so it never binds). The same source listed in this test's mocks: or in
// startup: binds, even where it was recorded in an earlier test's window:
// that edit is the one the line names. A mock mappings.yaml names in no
// place keeps where it was recorded.
func TestOutcomeAWorkerSourceIsRefusedNamingTheEdit(t *testing.T) {
	const p = "header.Authorization"
	scoped := func(run bool, s Scope) Input {
		in := oInput(p, "Bearer tok-9a1", "Bearer tok-9b2",
			oServed("mock-22", run, false, Response, oText("body.token", "tok-9a1"), "tok-9b2", time.Millisecond), nil)
		in.Scope = func(mock string) Scope {
			if mock == "mock-22" {
				return s
			}
			return ScopeUnknown
		}
		return in
	}
	d := oWord(t, Decide(scoped(false, ScopeWorker)), "9a1", "9b2")
	oRefused(t, "a token a worker: mock sent in the test's window", d, "another-scope", "mock-22")
	if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "worker:") {
		t.Errorf("a worker: source: why %q; want it to name worker:", d.Refusal.Why)
	}
	if !oEdits(d.Refusal, "move-to-mocks", "move-to-startup", "noise-at-word") {
		t.Errorf("a worker: source: edits %+v; want move to mocks:, move to startup:, noise at the word", d.Refusal)
	}
	d = oWord(t, Decide(scoped(false, ScopeTest("test-1"))), "9a1", "9b2")
	oRefused(t, "a token test-1's mock sent", d, "another-scope", "mock-22")
	if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "test-1") {
		t.Errorf("another test's source: why %q; want it to name test-1", d.Refusal.Why)
	}
	for _, s := range []Scope{ScopeTest("test-2"), ScopeStartup} {
		d = oWord(t, Decide(scoped(true, s)), "9a1", "9b2")
		if d.Outcome != OutcomeValue || d.From.Mock != "mock-22" {
			t.Errorf("a token an earlier test's recording sent, listed in %s: %s; want a Value from mock-22", s, oDescribe([]Decision{d}))
		}
	}
	d = oWord(t, Decide(scoped(true, ScopeUnknown)), "9a1", "9b2")
	oRefused(t, "a token an earlier test's recording sent, in no mappings place", d, "another-scope", "mock-22")
}

// A place before the call that keploy could not read (the test case's own
// request, or the run's places past its budget) refuses every change of the
// call, even one a readable place would bind: stricter than main, which
// binds a source it found before it reads the unread request. A
// client test has no test-case request, and that is not unread.
func TestOutcomeAnUnreadPlaceRefusesEveryChange(t *testing.T) {
	src := oServed("mock-22", false, false, Response, oText("body.id", "o-41"), "o-42", time.Millisecond)
	for _, tc := range []struct {
		what   string
		unread Earlier
	}{
		{"the test case's request", Earlier{Mock: "test-2", Given: true, At: oAt.Add(-time.Second), Unread: "keploy reads no request of a test case of kind X"}},
		{"the run past its budget", Earlier{Mock: "", Run: true, At: oAt.Add(-time.Second), Unread: "the run keeps no more of the set's places past 64 KiB"}},
	} {
		earlier := append([]Earlier{tc.unread}, src...)
		for _, w := range []struct{ rec, live, r, f string }{
			{"/v1/orders/o-41", "/v1/orders/o-42", "41", "42"},
			{"/v1/orders/z-5", "/v1/orders/z-6", "5", "6"},
		} {
			d := oWord(t, Decide(oInput("url.path", w.rec, w.live, earlier, nil)), w.r, w.f)
			oRefused(t, tc.what+" unread, "+w.rec, d, "unread")
		}
	}
	in := oInput("url.path", "/v1/orders/z-5", "/v1/orders/z-6", nil, nil)
	in.Drifts = oClientDrifts()
	if d := oWord(t, Decide(in), "5", "6"); d.Outcome != OutcomeFresh {
		t.Errorf("a client test, no test-case request: %s; want fresh, not unread", oDescribe([]Decision{d}))
	}
}

// An answer round F served only after the call reached keploy (#5018's
// arrival cut) is no place before the call in F: a word it would explain is
// refused as unknown, never fresh and never bound to it (at learn
// level).
func TestOutcomeAnAnswerServedAfterTheCallArrivedIsNoSource(t *testing.T) {
	late := oAnswer("mock-23", 100*time.Millisecond, "82").ServedAs(1).ServedAsRecorded()
	late.servedAfter = true
	d := oWord(t, Decide(oInput("bind.$1", "slot 82", "slot 16", nil, []Raw{late})), "82", "16")
	oRefused(t, "an answer F served after the call arrived", d, "unknown", "mock-23")
}

// The false green the cross-test word branch found: "82" to "16" beside a
// leaf the recording gave as 5150 and round F served as 16. The place is
// unchanged in neither sense: R's holds no 82, F's took F's new word only.
// Only F's value took its new word: unknown, refused, never bound and
// never fresh.
func TestOutcomeAWordOnlyFsPlaceTookIsRefused(t *testing.T) {
	gave := oServed("mock-24", false, false, Response, oText("body.port", "5150"), "16", time.Millisecond)
	d := oWord(t, Decide(oInput("body.slot", "slot-82", "slot-16", gave, nil)), "82", "16")
	oRefused(t, `"82" to "16" beside 5150 served as 16`, d, "unknown", "mock-24")
}

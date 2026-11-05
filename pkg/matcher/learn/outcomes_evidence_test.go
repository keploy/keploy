package learn

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	if !oEdits(d.Refusal, "move-to-mocks", "move-to-startup", "noise-at-field") {
		t.Errorf("a source between tests: edits %+v; want move to mocks:, move to startup:, noise at its field", d.Refusal)
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
	if !oEdits(d.Refusal, "move-to-mocks", "move-to-startup", "noise-at-field") {
		t.Errorf("a worker: source: edits %+v; want move to mocks:, move to startup:, noise at its field", d.Refusal)
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

// A recording another test's call, or a call recorded after this one, was
// answered with (round F served it before the call; Raw.OfAnotherTest, or
// one the set read once: Input.Given) is a place outside the call's scope.
// A word copied from it is refused, naming the mappings.yaml edit: never
// bound to it and never made anew each run. Where keploy cannot read the
// set's recordings back, every change of the call is refused.
func TestOutcomeAWordFromAnotherTestsRecordingIsRefusedNamingTheEdit(t *testing.T) {
	other := func() Raw {
		x := oAnswer("mock-31", 2*time.Second, "rk7302").ServedAs(3)
		return x.ServedLive(oAnswer("mock-31", 2*time.Second, "fq9915"))
	}
	const rec, live = "job rk7302 ready", "job fq9915 ready"
	in := oInput("body.note", rec, live, nil, []Raw{other().OfAnotherTest()})
	oRefused(t, "a word copied from another test's recording", oWord(t, Decide(in), "rk7302", "fq9915"), "another-scope", "mock-31")

	in = oInput("body.note", rec, live, nil, nil)
	in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(other())}, nil }
	oRefused(t, "a word copied from a recording the set gave before the call", oWord(t, Decide(in), "rk7302", "fq9915"), "another-scope", "mock-31")

	in.Given = func() ([]GivenPlaces, error) { return nil, errors.New("mock-31 is gone") }
	oRefused(t, "the set's recordings unread", oWord(t, Decide(in), "rk7302", "fq9915"), "unread", "mock-31 is gone")

	// A word no recording of the set holds is made anew each run.
	in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(other())}, nil }
	in.Rec, in.Live = oShape("body.note", "job ab1234 ready"), oShape("body.note", "job cd5678 ready")
	if d := oWord(t, Decide(in), "ab1234", "cd5678"); d.Outcome != OutcomeFresh {
		t.Errorf("a word no recording of the set holds: %s; want fresh", oDescribe([]Decision{d}))
	}
}

// A recording of the call's own scope round F served before the call,
// whose adapter did not say what F gave there (no view stated, not as
// recorded), may hold round F's new word: unknown, refused, never fresh
// (main refuses it too). So is one F served changed where one of its views
// holds that word. Not served, served as recorded, or served with views
// that hold neither word, it shows nothing, and the word is made anew.
func TestOutcomeARecordingFServedWithNoViewStatedLeavesTheWordUnknown(t *testing.T) {
	mk := func() Raw { return oAnswer("mock-32", time.Second, "ok") }
	view := func(w string) Raw { return oAnswer("mock-32", time.Second, w) }
	for _, c := range []struct {
		what    string
		x       Raw
		refused bool
	}{
		{"served, no view stated", mk().ServedAs(3), true},
		{"served changed, one view holds F's word", mk().ServedAs(3).ServedChanged(view("ok"), view("fq9915")), true},
		{"served changed, a view keploy could not read", mk().ServedAs(3).ServedChanged(), true},
		{"not served", mk().WithServed(false), false},
		{"served as recorded", mk().ServedAs(3).ServedAsRecorded(), false},
		{"served changed, no view holds either word", mk().ServedAs(3).ServedChanged(view("ok"), view("no")), false},
	} {
		d := oWord(t, Decide(oInput("body.note", "job rk7302 ready", "job fq9915 ready", nil, []Raw{c.x})), "rk7302", "fq9915")
		switch {
		case c.refused:
			oRefused(t, c.what, d, "unknown", "mock-32")
		case d.Outcome != OutcomeFresh:
			t.Errorf("%s: %s; want fresh", c.what, oDescribe([]Decision{d}))
		}
	}
}

// An instant that stands in an answer outside the call's scope, given
// before the call in both runs (another test's recording, or one recorded
// before the call that the set keeps elsewhere), and that its call's window
// holds too: a source outside the scope, refused naming the edit as a
// word's is, never a read of the clock. On main the first binds to the
// other test's answer and the second is a Clock: both stricter here. The
// call's own test's answer binds it. A recording of the set is read as
// recorded where round F gave it so; where F's serve of it was told with a
// view of its own (kept under the proven switch), F's moment stands in what
// F gave there, so the instant is refused naming the edit too, where main
// learns a Clock. All at a cut finer than a millisecond: at one that coarse,
// a moment its call's own window holds is that read (the row below).
func TestOutcomeAnInstantAnAnswerOutsideTheScopeGaveIsRefusedNamingTheEdit(t *testing.T) {
	const p = "body.seenAt"
	fAt := oAt.Add(time.Hour)
	rRead, fRead := oAt.Add(-40*time.Millisecond+17*time.Microsecond), fAt.Add(-40*time.Millisecond+17*time.Microsecond)
	rec, live := iMicro(rRead), iMicro(fRead)
	ans := func(v string) Raw {
		return oPg("mock-41", 30*time.Millisecond, "SELECT now()", []models.PostgresV3Cells{{{Value: v}}}, "SELECT 1")
	}
	served := func() Raw { return ans(rec).ServedAs(3).ServedLive(ans(live)) }
	call := func(raw []Raw) Input {
		return iCall(p, String, rec, live, oAt, fAt, iWin(oAt, fAt, 100*time.Millisecond), nil, raw)
	}
	for _, c := range []struct {
		what string
		x    Raw
	}{
		{"another test's answer", served().ServedInAnOlderTest()},
		{"a recording outside the test", served().OfAnotherTest()},
		{"a recording outside the test, F's view not stated", ans(rec).ServedAs(3).OfAnotherTest()},
	} {
		oRefused(t, "an instant "+c.what+" gave", oWord(t, Decide(call([]Raw{c.x})), rec, live), "another-scope", "mock-41")
	}
	iValue(t, "an instant the test's own answer gave", oWord(t, Decide(call([]Raw{served()})), rec, live), "mock-41")

	set := func(view bool) Input {
		g := PlacesOf(served())
		if view {
			g.fView = viewAnswerMoments(ans(live))
			for i := range g.fView {
				g.fView[i].at = g.at
			}
		}
		in := call(nil)
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
		return in
	}
	iClock(t, "an instant a recording of the set F gave as recorded holds", oWord(t, Decide(set(false)), rec, live))
	t.Setenv(provenswitch.Env, "1")
	oRefused(t, "an instant round F's view of a recording of the set holds", oWord(t, Decide(set(true)), rec, live), "another-scope", "mock-41")
}

// A recording of the set (another test's answer, read once for the set)
// that holds a short word of the value unchanged shows the word is chance
// only where it is before the call in both runs, as a recording handed to
// the call does: one with no answer time, or answered at the call's own
// instant, is unknown, refused naming why (chance_word_test.go:83). One
// answered after the call gave R's application nothing before it, so R's
// word there decides nothing; served as recorded, it holds no source of F's
// value: fresh (answerState, one rule however a recording is handed in).
func TestOutcomeASetRecordingWithNoTimeBeforeTheCallShowsNothing(t *testing.T) {
	const p = "body.appId"
	before := r1Insert.Add(-time.Minute)
	answered := func(res time.Time) Raw {
		m := receiveOf("mock-31", "bb7ed9ee-670e-48d1-982f-b54b2b73982f", before)
		m.Spec.ResTimestampMock = res
		return clockedDrifts().RawOf(m, true).ServedAs(3).ServedAsRecorded()
	}
	for _, c := range []struct {
		what, why string
		x         Raw
	}{
		{"answered before the call", "", answered(before.Add(time.Second))},
		{"no answer time", noAnswerAt, answered(time.Time{})},
		{"answered at the call's own instant", atCallWhy, answered(r1Insert)},
		{"answered after the call", "", answered(r1Insert.Add(time.Second))},
	} {
		x := c.x
		in := oInput(p, "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812", "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5", nil, nil)
		in.At, in.Arrived = r1Insert, r1Insert
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
		d := oWord(t, Decide(in), "48d1", "4c7d")
		switch {
		case c.why == "" && d.Outcome != OutcomeFresh:
			t.Errorf("%s: %s; want fresh (a short word by chance)", c.what, oDescribe([]Decision{d}))
		case c.why != "":
			oRefused(t, c.what, d, "unknown", "mock-31")
			if d.Refusal != nil && !strings.Contains(d.Refusal.Why, c.why) {
				t.Errorf("%s: %s; want it to say %q", c.what, d.Refusal.Why, c.why)
			}
		}
	}
}

// A recording round F served whose answer time keploy cannot read (none,
// or one before its own request) is never after the call: answered at any
// time, it may have given R's application the recorded word before the
// call, so its state is unknown and the word is refused naming why (fail
// closed), whether F served it as recorded or its view is not stated
// (integrations' Postgres TestLearn_AValueThatFlowedIsRefusedNotNoise: a
// token an HTTP answer held, its mock stamped with a request time alone).
// One asked at the call's own instant and answered a millisecond after it
// gave R's application nothing before the call; served as recorded, it
// holds no source of F's value: fresh, the answer-time rule's stated cost
// (that file's TestLearn_ARecordingTheLearnReplayServedInTheTest...'s
// subcase at the call's own instant).
func TestOutcomeAServedRecordingWithNoAnswerTimeIsNeverAfterTheCall(t *testing.T) {
	served := func(req, res time.Time, asRecorded bool) Raw {
		r := RawOf(&models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			ReqTimestampMock: req, ResTimestampMock: res,
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "tok-7f3a"},
		}}, true).ServedAs(1)
		if asRecorded {
			r = r.ServedAsRecorded()
		}
		return r
	}
	before := oAt.Add(-time.Second)
	for _, c := range []struct {
		what, why string
		x         Raw
	}{
		{"asked a second before, no answer time, its view not stated", noAnswerAt, served(before, time.Time{}, false)},
		{"asked a second before, no answer time, served as recorded", noAnswerAt, served(before, time.Time{}, true)},
		{"asked at the call's instant, no answer time, served as recorded", noAnswerAt, served(oAt, time.Time{}, true)},
		{"answered before its own request, served as recorded", noAnswerAt, served(before, before.Add(-time.Millisecond), true)},
		{"asked at the call's instant, answered a millisecond after, served as recorded", "", served(oAt, oAt.Add(time.Millisecond), true)},
	} {
		d := oWord(t, Decide(oInput("bind.$1", "tok-7f3a", "tok-0c41", nil, []Raw{c.x})), "7f3a", "0c41")
		if c.why == "" {
			if d.Outcome != OutcomeFresh {
				t.Errorf("%s: %s; want fresh (it gave the application nothing before the call)", c.what, oDescribe([]Decision{d}))
			}
			continue
		}
		oRefused(t, c.what, d, "unknown", "mock-1")
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, c.why) {
			t.Errorf("%s: %s; want it to say %q", c.what, d.Refusal.Why, c.why)
		}
	}
}

// A word glued to the bytes around it ("s17" of "BAs17") in a test case's
// request body that declares no media type shows nothing: keploy cannot
// tell a protobuf's printable tag and length from a word, so the word is
// unknown there, refused naming it (given_test.go:1211). The same bytes
// declared text are read by their words: "BAs17" is no word "s17".
func TestOutcomeAWordGluedInABodyOfNoDeclaredTypeIsUnknown(t *testing.T) {
	v17, v18 := "s17"+strings.Repeat("x", 62), "s18"+strings.Repeat("x", 62)
	framed := "BA" + v17 + "BA" + v18
	req := func(ct string) []Earlier {
		parts := []models.TestRequestPart{{Name: "body", Recorded: framed, Sent: framed}}
		if ct != "" {
			parts = append(parts, models.TestRequestPart{Name: "header.Content-Type", Recorded: ct, Sent: ct})
		}
		return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: parts})}
	}
	for _, c := range []struct{ what, rec, live string }{{"a word", "s17", "s18"}, {"a whole field", v17, v18}} {
		in := costCall(clockedDrifts(), "body.slot", c.rec, c.live, costAt, req(""), nil, nil, nil)
		d := oWord(t, Decide(in), c.rec, c.live)
		oRefused(t, c.what+" glued in a body of no media type", d, "unknown", "test-1")
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "declares no media type") {
			t.Errorf("%s: %s; want it to say the body declares no media type", c.what, d.Refusal.Why)
		}
	}
	d := oWord(t, Decide(costCall(clockedDrifts(), "body.slot", "s17", "s18", costAt, req("text/plain"), nil, nil, nil)), "s17", "s18")
	if d.Outcome != OutcomeFresh {
		t.Errorf("s17 glued in a body declared text: %s; want fresh", oDescribe([]Decision{d}))
	}
}

// An answer before the call in both runs (mock-42, round F's view not
// stated) holds the changed minute by its text, and an exchange recorded after
// the call (mock-44), which round F gave the application before it, holds
// round F's minute: binding the instant to mock-42 is then a guess between
// two sources, so it is refused, naming mock-44, as main refuses it
// (offset_test.go:2054), whether mock-44's field is a finer time cut or
// rounded to round F's minute (its first second included) or the minute
// itself. Where it is the minute itself, it is refused as a pick from
// mock-44 before any answer is read: a pick needs round F's moment at the
// call's cut. Without mock-44 holding round F's minute (it holds the
// recorded minute alone), mock-42 binds it. An answer that holds the
// minute only inside a time of it (11:14:45), which main binds by a cut,
// is no source a variable could hold: refused, naming it.
func TestOutcomeAnAnswersInstantBesideALaterRecordingFGaveIsRefused(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := recT.Format(minute), recT.Add(time.Minute).Format(minute)
	base := recT.Truncate(time.Minute)
	own := RawOf(sentAtReceive("mock-42", at.Add(-time.Second), base.Format(minute)), true).ServedAs(2)
	call := func(earlier []Earlier, raw []Raw) Input {
		return Input{Drifts: clockedDrifts(), Drift: []string{p}, Live: strLeaf(p, live), Rec: strLeaf(p, rec), At: at, Arrived: at,
			Earlier: earlier, Raw: append(raw, own), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
	}
	for _, sent := range []struct {
		what, v string
		kind    RefusalKind
	}{
		{"15:51:05, cut to F's minute", base.Add(65 * time.Second).Format(time.RFC3339), RefusalUnknown},
		{"15:51:00, cut to F's minute", base.Add(time.Minute).Format(time.RFC3339), RefusalUnknown},
		{"15:50:40, rounded to F's minute", base.Add(40 * time.Second).Format(time.RFC3339), RefusalUnknown},
		{"the minute 15:51 verbatim", base.Add(time.Minute).Format(minute), RefusalPick},
	} {
		for _, later := range []struct {
			what string
			at   time.Time
			own  bool
		}{
			{"later in the call's own test's window", at.Add(30 * time.Second), true},
			{"in a later test's window", at.Add(2 * time.Minute), false},
		} {
			nb := sentAtReceive("mock-44", later.at, sent.v)
			what := sent.what + ", recorded " + later.what
			d := oWord(t, Decide(call([]Earlier{leafedOf(nb, later.own, 1)}, nil)), rec, live)
			oRefused(t, what+", served leaf by leaf", d, sent.kind, "mock-44")
			d = oWord(t, Decide(call(nil, []Raw{RawOf(nb, true).ServedAs(1).OfAnotherTest()})), rec, live)
			oRefused(t, what+", handed as a recording", d, sent.kind, "mock-44")
		}
	}
	alone := sentAtReceive("mock-44", at.Add(30*time.Second), base.Add(10*time.Second).Format(time.RFC3339))
	iValue(t, "the recorded minute alone at the later recording", oWord(t, Decide(call([]Earlier{leafedOf(alone, true, 1)}, nil)), rec, live), "mock-42")
	own = RawOf(sentAtReceive("mock-42", at.Add(-time.Second), base.Add(45*time.Second).Format(time.RFC3339)), true).ServedAs(2)
	oRefused(t, "an answer holding the minute inside a time of it", oWord(t, Decide(call(nil, nil)), rec, live), RefusalNoClockRead, "mock-42", "another form")
}

// An exchange served leaf by leaf whose request was recorded after the call,
// which round F gave the application before it with no view of its answer
// stated, may hold round F's new word anywhere its leaves do not show: the
// word is unknown, refused naming it, as main refuses it
// (served_elsewhere_test.go:134). Said served as recorded, it shows nothing,
// and the word is made anew.
func TestOutcomeALateExchangeWithNoViewStatedLeavesAWordUnknown(t *testing.T) {
	const path = "bind.$8"
	m := jobsMock(chanceSrcRec, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt.Add(time.Second), eRecAt.Add(time.Second+time.Millisecond)
	for _, asRecorded := range []bool{false, true} {
		e := Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, AsRecorded: asRecorded, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}}
		in := Input{Drifts: clockedDrifts(), Drift: []string{path}, Rec: strLeaf(path, "82"), Live: strLeaf(path, "6c7d"),
			At: eRecAt, Arrived: eRecAt, Earlier: []Earlier{e}, Raw: eStartup(), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
		d := oWord(t, Decide(in), "82", "6c7d")
		if asRecorded {
			if d.Outcome != OutcomeFresh {
				t.Errorf("a late exchange said served as recorded: %s; want a value made anew", oDescribe([]Decision{d}))
			}
			continue
		}
		oRefused(t, "a late exchange with no view stated", d, "unknown", m.Name)
	}
}

// An exchange served leaf by leaf whose request was recorded at the call's
// own instant may have come before the call or after it, so the call is
// read both ways, as main reads it: as an earlier place of the test, and
// as a recording F gave before the call, where only what its adapter said F
// gave there counts. A word either reading refuses is refused: told
// nothing, told a view that holds F's new value, or told a view keploy
// cannot read. Told as recorded, or a view with no word of the value, the
// value is made anew each run.
func TestOutcomeAnExchangeAtTheCallsOwnInstantIsReadBothWays(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	m := jobsMock(chanceSrcRec, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt, eRecAt.Add(time.Millisecond)
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) {
			v := jobsMock(id, "9")
			v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
			return v, nil
		}
	}
	for _, c := range []struct {
		name    string
		tell    func(tr *Tracker)
		refused bool
	}{
		{"told nothing", func(*Tracker) {}, true},
		{"told as recorded", func(tr *Tracker) { tr.ServedMockAsRecorded(m) }, false},
		{"told a view with no word of the value", func(tr *Tracker) { tr.ServedMockLive(m, "a", view(chanceSrcRec)) }, false},
		{"told a view that holds F's value", func(tr *Tracker) { tr.ServedMockLive(m, "a", view("6c7d")) }, true},
		{"told a view keploy cannot read", func(tr *Tracker) {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return nil, errors.New("gone") })
		}, true},
	} {
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		tr.Served(tr.Placed().Test, Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: true, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}})
		c.tell(tr)
		pl := tr.Placed()
		in := Input{Drifts: clockedDrifts(), Drift: []string{path}, Live: live, Rec: rec, At: eRecAt, Arrived: eRecAt,
			Earlier: InServeOrder(pl.Earlier, []*models.Mock{m}, pl.Order), Raw: eStartup(), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
		d := oWord(t, Decide(in), "82", "6c7d")
		if !c.refused {
			if d.Outcome != OutcomeFresh {
				t.Errorf("FALSE RED: an exchange at the call's own instant, %s: %s; want a value made anew", c.name, oDescribe([]Decision{d}))
			}
			continue
		}
		oRefused(t, "an exchange at the call's own instant, "+c.name, d, RefusalUnknown, m.Name)
	}
}

// A recording made a second after the call gave R's application nothing
// before it, so R's word there decides nothing, however the recording is
// handed in (an exchange of the test, a start-up recording, a set entry):
// only what round F gave there counts. Served as recorded, or with a view
// that holds no word of the value, it holds no source of F's value, and the
// value is made anew each run. A view that holds F's value, one keploy
// cannot read, or none told, still refuses it, naming the recording.
func TestOutcomeARecordingMadeAfterTheCallDecidesNothingByItsRecordedWord(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	after := eRecAt.Add(time.Second)
	m := jobsMock("82", "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = after, after.Add(time.Millisecond)
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) {
			v := jobsMock(id, "9")
			v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
			return v, nil
		}
	}
	for _, c := range []struct {
		name    string
		tell    func(tr *Tracker)
		refused bool
	}{
		{"told nothing", func(*Tracker) {}, true},
		{"told as recorded", func(tr *Tracker) { tr.ServedMockAsRecorded(m) }, false},
		{"told a view with no word of the value", func(tr *Tracker) { tr.ServedMockLive(m, "a", view(chanceSrcRec)) }, false},
		{"told a view that holds F's value", func(tr *Tracker) { tr.ServedMockLive(m, "a", view("6c7d")) }, true},
		{"told a view keploy cannot read", func(tr *Tracker) {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return nil, errors.New("gone") })
		}, true},
	} {
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		tr.Served(tr.Placed().Test, Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: true, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}})
		c.tell(tr)
		pl := tr.Placed()
		in := Input{Drifts: clockedDrifts(), Drift: []string{path}, Live: live, Rec: rec, At: eRecAt, Arrived: eRecAt,
			Earlier: InServeOrder(pl.Earlier, []*models.Mock{m}, pl.Order), Raw: eStartup(), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
		d := oWord(t, Decide(in), "82", "6c7d")
		if !c.refused {
			if d.Outcome != OutcomeFresh {
				t.Errorf("FALSE RED: an exchange recorded after the call, %s: %s; want a value made anew", c.name, oDescribe([]Decision{d}))
			}
			continue
		}
		oRefused(t, "an exchange recorded after the call, "+c.name, d, RefusalUnknown, m.Name)
	}
	rows := []models.PostgresV3Cells{{{Value: "82"}, {Value: "5"}}}
	late := ePg("mock-5", -time.Second, "SELECT id, n FROM jobs", rows, "SELECT 1").ServedAs(6)
	for _, c := range []struct {
		name    string
		raw     Raw
		refused bool
	}{
		{"served at the start-up with no view told", late, true},
		{"served at the start-up as recorded", late.ServedAsRecorded(), false},
		{"served at the start-up with a view that holds no word of the value",
			late.ServedLive(ePg("mock-5", -time.Second, "SELECT id, n FROM jobs", []models.PostgresV3Cells{{{Value: chanceSrcRec}, {Value: "9"}}}, "SELECT 1")), false},
		{"served at the start-up with a view that holds F's value",
			late.ServedLive(ePg("mock-5", -time.Second, "SELECT id, n FROM jobs", []models.PostgresV3Cells{{{Value: "6c7d"}, {Value: "9"}}}, "SELECT 1")), true},
	} {
		in := Input{Drifts: clockedDrifts(), Drift: []string{path}, Live: live, Rec: rec, At: eRecAt, Arrived: eRecAt,
			Raw: append(eStartup(), c.raw), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
		d := oWord(t, Decide(in), "82", "6c7d")
		if !c.refused {
			if d.Outcome != OutcomeFresh {
				t.Errorf("FALSE RED: a start-up recording made after the call, %s: %s; want a value made anew", c.name, oDescribe([]Decision{d}))
			}
			continue
		}
		oRefused(t, "a start-up recording made after the call, "+c.name, d, RefusalUnknown, "mock-5")
	}
}

// answeredAtInput is the call changing "82" to "6c7d" beside one recording
// that holds "82", asked at req and answered at res, handed in as an
// exchange of the test (its leaf holds "82"), as a start-up recording or as
// a set entry; round F served it as recorded, or with a view that holds
// "6c7d" where fView. mock-6 is the exchange's name, mock-5 the others'.
func answeredAtInput(how string, req, res time.Time, fView bool) Input {
	const path = "bind.$8"
	in := Input{Drifts: clockedDrifts(), Drift: []string{path}, At: eRecAt, Arrived: eRecAt, Raw: eStartup(), Test: "test-2", Mock: "mock-300",
		Rec:  Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}},
		Live: Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}},
		Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
	pg := func(id string) Raw {
		return ePgAnswered("mock-5", eRecAt.Sub(req), res, "SELECT id, n FROM jobs", []models.PostgresV3Cells{{{Value: id}, {Value: "5"}}}, "SELECT 1")
	}
	x := pg("82").ServedAs(6).ServedAsRecorded()
	if fView {
		x = pg("82").ServedAs(6).ServedLive(pg("6c7d"))
	}
	switch how {
	case "an exchange of the test":
		m := jobsMock("82", "5")
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = req, res
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		tr.Served(tr.Placed().Test, Earlier{Mock: m.Name, At: req, Own: true, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.id", Type: String, Value: "82"}, Live: map[bool]string{false: "82", true: "6c7d"}[fView]}}})
		if fView {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) {
				v := jobsMock("6c7d", "5")
				v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = req, res
				return v, nil
			})
		} else {
			tr.ServedMockAsRecorded(m)
		}
		pl := tr.Placed()
		in.Earlier = InServeOrder(pl.Earlier, []*models.Mock{m}, pl.Order)
	case "a start-up recording":
		in.Raw = append(in.Raw, x)
	default:
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
	}
	return in
}

// answeredAtHow are the ways a recording is handed to the call.
var answeredAtHow = []string{"an exchange of the test", "a start-up recording", "a set entry"}

// A recording is placed by its answer, when the application got its words,
// however it is handed in (an exchange of the test, a start-up recording, a
// set entry): answered before the call, it is before it; answered after
// the call, however its request was timed, it gave R's application nothing
// before the call, so R's word there decides nothing; and the call's own
// instant is neither before nor after it: a recording answered at it is
// unknown, refused naming it (fail closed). Each holds R's word and round
// F served it as recorded, so only the instant refuses: the others hold no
// source of F's value, and the value is made anew each run.
func TestOutcomeARecordingIsPlacedByItsAnswerHoweverItIsHandedIn(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		name     string
		req, res time.Time
		refused  bool
	}{
		{"asked and answered before the call", eRecAt.Add(-20 * ms), eRecAt.Add(-19 * ms), false},
		{"asked before the call and answered 10ms after it", eRecAt.Add(-10 * ms), eRecAt.Add(10 * ms), false},
		{"asked before the call and answered a second after it", eRecAt.Add(-10 * ms), eRecAt.Add(time.Second), false},
		{"asked and answered after the call", eRecAt.Add(time.Second), eRecAt.Add(time.Second + ms), false},
		{"asked at the call's own instant and answered after it", eRecAt, eRecAt.Add(ms), false},
		{"asked before the call and answered at its own instant", eRecAt.Add(-10 * ms), eRecAt, true},
		{"asked and answered at the call's own instant", eRecAt, eRecAt, true},
	} {
		for _, how := range answeredAtHow {
			d := oWord(t, Decide(answeredAtInput(how, c.req, c.res, false)), "82", "6c7d")
			what := how + " " + c.name + ", served as recorded"
			if !c.refused {
				if d.Outcome != OutcomeFresh {
					t.Errorf("FALSE RED: %s: %s; want a value made anew", what, oDescribe([]Decision{d}))
				}
				continue
			}
			oRefused(t, what, d, RefusalUnknown, map[bool]string{true: "mock-6", false: "mock-5"}[how == answeredAtHow[0]], atCallWhy)
		}
	}
	// An exchange keploy was given no payload of is placed by its request:
	// at the call's own instant, unknown.
	in := answeredAtInput(answeredAtHow[1], eRecAt.Add(-time.Second), eRecAt.Add(-time.Second+ms), false)
	in.Earlier = []Earlier{{Mock: "mock-77", At: eRecAt, Own: true, Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.id", Type: String, Value: "82"}, Live: "82"}}}}
	oRefused(t, "an exchange with no payload at the call's own instant", oWord(t, Decide(in), "82", "6c7d"), RefusalUnknown, "mock-77", atCallWhy)
	// A recording answered after the call that round F never served holds
	// no source of F's value either.
	in = answeredAtInput(answeredAtHow[1], eRecAt.Add(time.Second), eRecAt.Add(time.Second+ms), false)
	in.Raw[len(in.Raw)-1] = ePgAnswered("mock-5", -time.Second, eRecAt.Add(time.Second+ms), "SELECT id, n FROM jobs", []models.PostgresV3Cells{{{Value: "82"}, {Value: "5"}}}, "SELECT 1")
	if d := oWord(t, Decide(in), "82", "6c7d"); d.Outcome != OutcomeFresh {
		t.Errorf("FALSE RED: a recording answered after the call that round F never served: %s; want a value made anew", oDescribe([]Decision{d}))
	}
}

// A word refused at a recording answered after the call is refused by round
// F's side, never by R's: R's word there decides nothing. So its line says
// what round F gave there, not that the recording gave the application
// nothing (why R's word decides nothing), whichever way the recording is
// handed in and however its request was timed: round F's view there holds
// the new value, or round F served it with no view told.
func TestOutcomeAWordRefusedAtARecordingAnsweredAfterTheCallSaysWhatRoundFGave(t *testing.T) {
	const fSide = "answered after the call, it shows nothing of the recorded value; round F gave, or may have given, the new value there"
	ms := time.Millisecond
	check := func(what string, d Decision, mock string) {
		t.Helper()
		oRefused(t, what, d, RefusalUnknown, mock, fSide)
		if strings.Contains(d.Refusal.Why, "did not give the application") {
			t.Errorf("%s: refused %q; its reason says the recording gave nothing, which refuses nothing", what, d.Refusal.Why)
		}
	}
	for _, c := range []struct {
		name     string
		req, res time.Time
	}{
		{"asked before the call and answered after it", eRecAt.Add(-10 * ms), eRecAt.Add(10 * ms)},
		{"asked and answered after the call", eRecAt.Add(time.Second), eRecAt.Add(time.Second + ms)},
	} {
		for _, how := range answeredAtHow {
			d := oWord(t, Decide(answeredAtInput(how, c.req, c.res, true)), "82", "6c7d")
			check(how+" "+c.name+", round F's view holding the new value", d, map[bool]string{true: "mock-6", false: "mock-5"}[how == answeredAtHow[0]])
		}
		in := answeredAtInput(answeredAtHow[1], c.req, c.res, false)
		in.Raw[len(in.Raw)-1] = ePgAnswered("mock-5", eRecAt.Sub(c.req), c.res, "SELECT id, n FROM jobs", []models.PostgresV3Cells{{{Value: "82"}, {Value: "5"}}}, "SELECT 1").ServedAs(6)
		check("a start-up recording "+c.name+", served with no view told", oWord(t, Decide(in), "82", "6c7d"), "mock-5")
	}
}

// An answer of the call's own test holds the changed minute by its text (step 3), but
// a second place may be where it came from: a recording of the set F gave
// in an earlier test that was recorded after the call, or at its own
// instant, whose answer names F's minute (or, at its own instant, R's);
// or another test's answer given before the call whose view in round F
// keploy cannot tell, which names R's minute. The bind would be a guess
// between the two: refused, naming the second place. A recording made after
// the call that holds only R's minute is none: the own answer binds.
func TestOutcomeAnAnswersInstantBesideASecondPossibleSourceIsRefused(t *testing.T) {
	const p = "body.sent_at"
	at := r1Insert
	recT := at.Truncate(time.Second)
	const minute = "2006-01-02T15:04Z07:00"
	rec, live := recT.Format(minute), recT.Add(time.Minute).Format(minute)
	base := recT.Truncate(time.Minute)
	own := RawOf(sentAtReceive("mock-42", at.Add(-time.Minute), base.Format(minute)), true).ServedAs(3)
	call := func(raw []Raw, set ...Raw) Input {
		in := Input{Drifts: clockedDrifts(), Drift: []string{p}, Live: strLeaf(p, live), Rec: strLeaf(p, rec), At: at, Arrived: at,
			Raw: append(raw, own), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
		if len(set) > 0 {
			in.Given = func() ([]GivenPlaces, error) {
				var gs []GivenPlaces
				for _, x := range set {
					gs = append(gs, PlacesOf(x))
				}
				return gs, nil
			}
		}
		return in
	}
	for _, when := range []struct {
		what string
		at   time.Time
	}{{"recorded after the call", at.Add(3 * time.Minute)}, {"recorded at the call's own instant", at}} {
		for _, sent := range []struct {
			what    string
			off     time.Duration
			refused bool
		}{
			{"F's minute alone", 65 * time.Second, true},
			{"rounds to F's minute", 40 * time.Second, true},
			{"the recorded minute alone", 10 * time.Second, when.at.Equal(at)},
		} {
			nb := RawOf(sentAtReceive("mock-44", when.at, base.Add(sent.off).Format(time.RFC3339)), true).ServedAs(1)
			d := oWord(t, Decide(call(nil, nb)), rec, live)
			what := "a set recording " + when.what + ", " + sent.what
			if sent.refused {
				oRefused(t, what, d, RefusalUnknown, "mock-44", "mock-42")
				continue
			}
			iValue(t, what, d, "mock-42")
		}
	}
	nb := RawOf(sentAtReceive("mock-43", at.Add(-2*time.Minute), base.Add(10*time.Second).Format(time.RFC3339)), true).ServedAs(1)
	oRefused(t, "another test's answer, view not stated, handed as a recording", oWord(t, Decide(call([]Raw{nb.OfAnotherTest()})), rec, live), RefusalUnknown, "mock-43", "mock-42")
	oRefused(t, "another test's answer, view not stated, in the set", oWord(t, Decide(call(nil, nb)), rec, live), RefusalUnknown, "mock-43", "mock-42")
	asRec := nb.ServedAsRecorded()
	iValue(t, "another test's answer served as recorded, handed as a recording", oWord(t, Decide(call([]Raw{asRec.OfAnotherTest()})), rec, live), "mock-42")
	iValue(t, "another test's answer served as recorded, in the set", oWord(t, Decide(call(nil, asRec)), rec, live), "mock-42")
}

// A place keploy could not read names itself and why in the refusal's line:
// the store's error where a recording could not be loaded, the reason a
// place past the budget gives, and a name where the place carries none (a
// recording of the test, the run's values, the test case's request), never
// an empty one.
func TestOutcomeAnUnreadPlaceNamesItselfAndWhy(t *testing.T) {
	before := oAt.Add(-time.Second)
	for _, tc := range []struct {
		what    string
		earlier []Earlier
		raw     []Raw
		names   []string
	}{
		{"a recording the store could not load", nil, []Raw{{mock: "mock-7", unread: "disk mocks: store closed"}},
			[]string{"keploy could not read mock-7 (disk mocks: store closed)"}},
		{"an unnamed recording the store could not load", nil, []Raw{{unread: "disk mocks: store closed"}},
			[]string{"keploy could not read a recording of the test (disk mocks: store closed)"}},
		{"a recording never read from its mock", nil, []Raw{{mock: "mock-8"}},
			[]string{"keploy could not read mock-8 (it was not read from its mock, learn.RawOf)"}},
		{"the run past its budget", []Earlier{{Run: true, At: before, Unread: "the run keeps no more of the set's places past 64 KiB"}}, nil,
			[]string{"keploy could not read the run's values (the run keeps no more of the set's places past 64 KiB)"}},
		{"an unnamed test case's request", []Earlier{{Given: true, At: before, Unread: "none was sent"}}, nil,
			[]string{"keploy could not read the test case's request (none was sent)"}},
		{"a named earlier recording", []Earlier{{Mock: "mock-9", At: before, Unread: "disk mocks: store closed"}}, nil,
			[]string{"keploy could not read mock-9 (disk mocks: store closed)"}},
	} {
		d := oWord(t, Decide(oInput("url.path", "/v1/orders/z-5", "/v1/orders/z-6", tc.earlier, tc.raw)), "5", "6")
		oRefused(t, tc.what, d, "unread", tc.names...)
		if d.Refusal != nil && strings.Contains(d.Refusal.Why, "read  (") {
			t.Errorf("%s: refused %q; it names no place", tc.what, d.Refusal.Why)
		}
	}
}

// An exchange round F served before the call that comes with no leaf and no
// payload keploy read (its adapter gave none, or keploy could not read it
// from its mock) is a place keploy cannot search, never one it drops: a word
// changed beside it is refused naming it, never made anew each run
// (offset_test.go:146 on main).
func TestOutcomeAnExchangeWithNoLeafAndNoPayloadIsUnread(t *testing.T) {
	m := &models.Mock{Name: "mock-11", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: "/slots"},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"slot":"slot-7"}`},
	}}
	for name, payload := range map[string]Raw{"none given": {}, "unread": unreadPayload(m)} {
		e := Earlier{Mock: "mock-11", At: oAt.Add(-time.Second), Payload: payload}
		d := oWord(t, Decide(iCall("body.slot", String, "slot-7", "slot-9", oAt, oAt, nil, []Earlier{e}, nil)), "7", "9")
		oRefused(t, name, d, RefusalUnread, "mock-11")
	}
}

// An audit's second, its call's own read of the clock, that an earlier
// test's answer also holds: a fast recording ran several tests in one
// second, so another test's document stamped in it is read back before the
// call. That share flips with the runs' timing. At a millisecond or coarser,
// where the call's own window holds the read, it is that read, a Clock;
// where its window does not, or the answer holds the moment at a cut finer
// than a millisecond (a pick from it, not two stamps in one cut), it stays
// refused naming the edit.
func TestOutcomeASecondAnotherTestsAnswerAlsoHoldsIsTheCallsOwnRead(t *testing.T) {
	const p = "body.docs.msgs.0.auditedAt"
	fAt := oAt.Add(time.Hour)
	rRead, fRead := oAt.Add(-40*time.Millisecond), fAt.Add(-40*time.Millisecond)
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	ans := func(v string) Raw {
		return oPg("mock-1200", 30*time.Millisecond, "SELECT created_at", []models.PostgresV3Cells{{{Value: v}}}, "SELECT 1")
	}
	forms := func(rec, live string) []struct {
		what string
		x    Raw
	} {
		served := func() Raw { return ans(rec).ServedAs(3).ServedLive(ans(live)) }
		return []struct {
			what string
			x    Raw
		}{
			{"another test's answer", served().ServedInAnOlderTest()},
			{"a recording outside the test", served().OfAnotherTest()},
			{"a recording outside the test, F's view not stated", ans(rec).ServedAs(3).OfAnotherTest()},
		}
	}
	own := iWin(oAt, fAt, 100*time.Millisecond)
	far := iWin(oAt.Add(5*time.Second), fAt.Add(5*time.Second), 100*time.Millisecond)
	rec, live := sec(rRead), sec(fRead)
	for _, c := range forms(rec, live) {
		iClock(t, "a second "+c.what+" also holds", oWord(t, Decide(iCall(p, Number, rec, live, oAt, fAt, own, nil, []Raw{c.x})), rec, live))
		oRefused(t, "a second "+c.what+" holds, its window does not", oWord(t, Decide(iCall(p, Number, rec, live, oAt, fAt, far, nil, []Raw{c.x})), rec, live), "another-scope", "mock-1200")
	}
	rec, live = iMicro(rRead), iMicro(fRead)
	for _, c := range forms(rec, live) {
		oRefused(t, "a microsecond "+c.what+" holds", oWord(t, Decide(iCall(p, String, rec, live, oAt, fAt, own, nil, []Raw{c.x})), rec, live), "another-scope", "mock-1200")
	}
}

// A start-up answer round F served as recorded, asked in a session of its
// own (every driver opens one, so the request changed): a fresh id's 12-bit
// group stands in one of its seeded ids by chance, about once in 38. At the
// field that holds the word the place is unchanged, so the piece is chance
// and the id fresh, whichever run's id holds it; a word that counts by its
// text there is a conflict. Where round F's answer holds round F's word and
// the recording's holds none, or gave round F's id whole, it is refused.
func TestOutcomeAPieceAStartUpsAnswerHoldsInASessionOfItsOwnIsChance(t *testing.T) {
	const (
		recSID, liveSID = "5Xa0KmL0RxqLFfFyLtYhPw", "w6Hg1H8rSmm44V0Mn3o7Jg"
		seeded, other   = "bb7ed9ee-670e-48d1-982f-b54b2b73982f", "0f1e2d3c-5b6a-4978-8a9b-cdef01234567"
		plain, holding  = "1111aaaa-2222-4333-9444-444455556666", "9999cccc-7777-48d1-8888-000011112222"
		long            = "bb7ed9ee-7777-4abc-8888-000011112222"
	)
	ans := func(sid string, ids ...string) Raw {
		rows := make([]models.PostgresV3Cells, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, models.PostgresV3Cells{{Value: id}})
		}
		return oPg("mock-0", 2*time.Second, "SELECT _id FROM suites /* session "+sid+" */", rows, "SELECT 2")
	}
	call := func(rec, live string, boot Raw) []Decision {
		return Decide(oInput("body.cmd.documents.0._id", rec, live, nil, []Raw{boot}))
	}
	asRecorded := func(sid string) Raw {
		return ans(recSID, seeded, other).ServedAs(1).ServedLive(ans(sid, seeded, other))
	}
	for _, c := range []struct {
		what, rec, live, sid string
	}{
		{"round F's id holds the piece, asked in the recorded session", plain, holding, recSID},
		{"round F's id holds the piece, asked in a session of its own", plain, holding, liveSID},
		{"the recorded id holds the piece, round F's own session", holding, plain, liveSID},
	} {
		r, f := "4333", "48d1"
		if c.rec == holding {
			r, f = f, r
		}
		d := oWord(t, call(c.rec, c.live, asRecorded(c.sid)), r, f)
		if d.Outcome != OutcomeFresh {
			t.Errorf("%s: %s; want fresh, a 12-bit piece at the start-up's answer as recorded is chance", c.what, oDescribe([]Decision{d}))
		}
	}
	oRefused(t, "round F's 32-bit word at the answer as recorded, its own session", oWord(t, call(plain, long, asRecorded(liveSID)), "1111aaaa", "bb7ed9ee"), RefusalConflict, "mock-0")
	onlyF := ans(recSID, other).ServedAs(1).ServedLive(ans(liveSID, other, "c0de0000-1111-48d1-8222-333344445555"))
	oRefused(t, "round F's answer holds round F's word, the recording's none", oWord(t, call(plain, holding, onlyF), "4333", "48d1"), RefusalUnknown, "mock-0")
	whole := ans(recSID, seeded, other).ServedAs(1).ServedLive(ans(liveSID, holding, other))
	if d := call(plain, holding, whole); len(d) == 0 || d[0].Outcome != OutcomeRefused {
		t.Errorf("FALSE GREEN? round F's answer gave round F's id whole: %s; want refused", oDescribe(d))
	}
}

// Decide hands an exchange of the test whose request was recorded after the
// call in as a recording (leafedAfterCall), which answerState places. placesOf
// alone places it by the same rule, so no caller reads its leaves as before
// the call: not before, R's word deciding nothing, with the reason that says
// what round F gave there. The test case's own request (Given) is before the
// call in both runs, whatever its time.
func TestOutcomeAnExchangeAskedAfterTheCallIsPlacedAsARecordingIs(t *testing.T) {
	late := eRecAt.Add(time.Second)
	in := answeredAtInput(answeredAtHow[1], late, late.Add(time.Millisecond), false)
	seen := []Seen{{Side: Request, Leaf: Leaf{Path: "q.id", Type: String, Value: "82"}, Live: "82"}}
	in.Earlier = []Earlier{{Mock: "mock-77", At: late, Own: true, Seen: seen}, {At: late, Given: true, Seen: seen}}
	ps, _ := placesOf(in)
	found := 0
	for _, p := range ps {
		switch {
		case p.mock == "mock-77":
			found++
			if p.state != stateNotBefore || !p.recAfter || p.why != askedAfterWhy {
				t.Errorf("an exchange asked after the call: state %v, recorded after %v, why %q; want not before, R's word deciding nothing, why %q", p.state, p.recAfter, p.why, askedAfterWhy)
			}
		case p.given:
			found++
			if p.state != stateBefore || p.recAfter {
				t.Errorf("the test case's request: state %v, recorded after %v; want before in both runs", p.state, p.recAfter)
			}
		}
	}
	if found != 2 {
		t.Errorf("placesOf gave %d of the two exchanges' places; want both", found)
	}
}

// An object glued to a value beside it (`{"a":W,"b":x}"z"`, or `"z"{...}`)
// is two values, as a stream of values is: an answer whose word moved
// within the object between the runs does not show it at one field, so it
// proves no link and the word is refused, never bound there. A word that
// stayed at its field binds, as in any body. The reader reads the two
// values apart; this is the verdict Decide gives on it.
func TestOutcomeAnObjectGluedToAValueIsTwoValues(t *testing.T) {
	answer := func(body string) Raw {
		return RawOf(getAnswer("mock-6", "http://api/items/1", body, eRecAt.Add(-100*time.Millisecond)), true)
	}
	for _, c := range []struct{ name, r, f string }{
		{"an object glued to a string", `{"a":"48d1","b":"x"}"z"`, `{"a":"%s","b":"%s"}"z"`},
		{"a string glued to an object", `"z"{"a":"48d1","b":"x"}`, `"z"{"a":"%s","b":"%s"}`},
	} {
		call := func(f string) Decision {
			raw := append(eStartup(), answer(c.r).ServedAs(6).ServedLive(answer(f)))
			return oWord(t, Decide(costF("k-48d1", "k-6c7d", nil, raw, nil)), "48d1", "6c7d")
		}
		oRefused(t, c.name+", the word moved within the object", call(fmt.Sprintf(c.f, "9998", "6c7d")), RefusalUnknown, "mock-6")
		iValue(t, c.name+", the word stayed at its field", call(fmt.Sprintf(c.f, "6c7d", "x")), "mock-6")
	}
}

// tokenMock is a recording whose request alone holds word (an UPDATE that
// sends a token; its answer is a tag), asked at req and answered at res.
func tokenMock(name, word string, req, res time.Time) *models.Mock {
	return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: req, ResTimestampMock: res,
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "UPDATE tokens SET t = '" + word + "' WHERE id = 1",
			Response:      &models.PostgresV3Response{CommandComplete: "UPDATE 1"},
		}},
	}}
}

// requestWordInput is the call changing "82" to "6c7d" beside one
// recording whose request alone holds "82" (tokenMock), asked at req and
// answered at res: an exchange of the test whose request leaf round F sent
// as "6c7d" (mock-6), or a start-up recording (mock-5) round F served with
// a view whose request sent "6c7d", served as recorded, or never served.
func requestWordInput(how string, req, res time.Time) Input {
	return requestWordInputOf(how, "82", "6c7d", "82", "6c7d", req, res)
}

// requestWordInputOf is requestWordInput's call changing rec to live, its
// recording's request holding r where round F's view and exchange leaf
// hold f.
func requestWordInputOf(how, rec, live, r, f string, req, res time.Time) Input {
	const path = "bind.$8"
	in := Input{Drifts: clockedDrifts(), Drift: []string{path}, At: eRecAt, Arrived: eRecAt, Raw: eStartup(), Test: "test-2", Mock: "mock-300",
		Rec:  Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: rec}}},
		Live: Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: live}}},
		Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
	raw := func(word string, served bool) Raw { return RawOf(tokenMock("mock-5", word, req, res), served) }
	switch how {
	case "an exchange of the test":
		m := tokenMock("mock-6", r, req, res)
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		tr.Served(tr.Placed().Test, Earlier{Mock: m.Name, At: req, Own: true, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "q.t", Type: String, Value: r}, Live: f}}})
		tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return tokenMock("mock-6", f, req, res), nil })
		pl := tr.Placed()
		in.Earlier = InServeOrder(pl.Earlier, []*models.Mock{m}, pl.Order)
	case "a start-up recording whose view sent the new value":
		in.Raw = append(in.Raw, raw(r, true).ServedAs(6).ServedLive(raw(f, true)))
	case "a start-up recording served as recorded":
		in.Raw = append(in.Raw, raw(r, true).ServedAs(6).ServedAsRecorded())
	default:
		in.Raw = append(in.Raw, raw(r, false))
	}
	return in
}

// requestWordHow are the ways requestWordInput hands its recording in.
var requestWordHow = []string{"an exchange of the test", "a start-up recording whose view sent the new value",
	"a start-up recording served as recorded", "a start-up recording never served"}

// A word that stands only in a recording's request is what the recorded
// application sent, so it is placed by the request's time, however the
// recording is handed in, whenever it was answered: the application held
// the word when it sent it. Asked a second before the call and answered two
// seconds after it, or with no answer time keploy can read, the recording
// is read as one asked and answered before the call: round F's view that
// sent the new value at its field is where it came from (bound), one served
// as recorded sets the word aside (fresh), and one never served may hold
// round F's word (unknown, refused). Asked at the call's own instant, it is
// unknown and refused naming that, as an exchange's request leaf is. Asked
// after the call, with or without an answer time, it gave the application
// nothing before it, as now. Never
// fresh by a request answered late, which would leave unchecked a word the
// application sent before the call.
func TestOutcomeAWordARecordingsRequestHoldsIsPlacedByWhenItWasSent(t *testing.T) {
	ms := time.Millisecond
	// The request's time places it, so its line says it was asked after
	// the call, which keploy knows with no answer time too.
	const fSide = "asked after the call, it shows nothing of the recorded value; round F gave, or may have given, the new value there"
	mockOf := func(how string) string {
		return map[bool]string{true: "mock-6", false: "mock-5"}[how == requestWordHow[0]]
	}
	sentBefore := func(what, how string, d Decision) {
		t.Helper()
		switch how {
		case requestWordHow[0], requestWordHow[1]:
			iValue(t, what, d, mockOf(how))
		case requestWordHow[2]:
			if d.Outcome != OutcomeFresh {
				t.Errorf("%s: %s; want fresh (served as recorded, the word set aside)", what, oDescribe([]Decision{d}))
			}
		default:
			oRefused(t, what, d, RefusalUnknown, "mock-5")
		}
	}
	// Asked after the call, it gave the recorded application nothing
	// before it, whether or not its answer time can be read: only round
	// F's side decides.
	sentAfter := func(what, how string, d Decision) {
		t.Helper()
		switch how {
		case requestWordHow[0], requestWordHow[1]:
			oRefused(t, what, d, RefusalUnknown, mockOf(how), fSide)
		default:
			if d.Outcome != OutcomeFresh {
				t.Errorf("%s: %s; want fresh (it gave the application nothing before the call)", what, oDescribe([]Decision{d}))
			}
		}
	}
	for _, c := range []struct {
		name     string
		req, res time.Time
		check    func(what, how string, d Decision)
	}{
		{"asked a second before the call and answered 0.9s before it", eRecAt.Add(-time.Second), eRecAt.Add(-900 * ms), sentBefore},
		{"asked a second before the call and answered two seconds after it", eRecAt.Add(-time.Second), eRecAt.Add(2 * time.Second), sentBefore},
		{"asked a second before the call, with no answer time", eRecAt.Add(-time.Second), time.Time{}, sentBefore},
		{"asked at the call's own instant and answered 1ms after it", eRecAt, eRecAt.Add(ms), func(what, how string, d Decision) {
			t.Helper()
			oRefused(t, what, d, RefusalUnknown, mockOf(how), atCallWhy)
		}},
		{"asked a second after the call", eRecAt.Add(time.Second), eRecAt.Add(time.Second + ms), sentAfter},
		{"asked a second after the call, with no answer time", eRecAt.Add(time.Second), time.Time{}, sentAfter},
	} {
		for _, how := range requestWordHow {
			c.check(how+" "+c.name, how, oWord(t, Decide(requestWordInput(how, c.req, c.res)), "82", "6c7d"))
		}
	}
	// A start-up recording with no time keploy can read is unknown, refused
	// naming that, never read as one made at the call's own instant.
	for _, how := range requestWordHow[1:] {
		d := oWord(t, Decide(requestWordInput(how, time.Time{}, time.Time{})), "82", "6c7d")
		oRefused(t, how+" with no time keploy can read", d, RefusalUnknown, "mock-5", noAnswerAt)
		if d.Refusal != nil && strings.Contains(d.Refusal.Why, atCallWhy) {
			t.Errorf("%s with no time keploy can read: refused %q; want no instant named", how, d.Refusal.Why)
		}
	}
	// One round F served only after the call reached keploy is no place
	// before the call in F, whenever its request was sent: never bound.
	in := requestWordInput(requestWordHow[1], eRecAt.Add(-time.Second), eRecAt.Add(2*time.Second))
	in.Raw[len(in.Raw)-1].servedAfter = true
	oRefused(t, "a start-up recording round F served after the call arrived", oWord(t, Decide(in), "82", "6c7d"), RefusalUnknown, "mock-5")
}

// A word a recording's request holds is placed by when it was sent however
// the call's value carries it, asked a second before the call and answered
// two seconds after it as when answered before it: a whole URL-safe token
// a start-up recording's view sent binds there, and an id of 32 bits a
// never-served start-up recording's request holds, which the value carries
// only in a decoded form, is refused naming that recording, which may hold
// round F's id.
func TestOutcomeARequestWordIsPlacedByWhenItWasSentInATokenAndADecodedForm(t *testing.T) {
	const tr, tf = "Sfl-KxwRJ_SMeKKF2QT4fwpMeJf36POk6yJV_adQssw", "Q1x_Yp-9vTq3LmN0Rr4sWkHd2cJ7aBfE5gUoZ_iXyPq"
	const r32, f32 = "a1b2c3d4", "e5f6a7b8"
	enc := oEncoded()["a signed message"]
	req := eRecAt.Add(-time.Second)
	for _, c := range []struct {
		name string
		res  time.Time
	}{
		{"answered 0.9s before the call", eRecAt.Add(-900 * time.Millisecond)},
		{"answered two seconds after the call", eRecAt.Add(2 * time.Second)},
	} {
		d := oWord(t, Decide(requestWordInputOf(requestWordHow[1], tr, tf, tr, tf, req, c.res)), tr, tf)
		iValue(t, "a whole token a start-up recording's view sent, asked a second before the call and "+c.name, d, "mock-5")
		ds := Decide(requestWordInputOf(requestWordHow[3], enc(r32), enc(f32), r32, f32, req, c.res))
		if len(ds) == 0 {
			t.Errorf("an id in a decoded form, %s: no decision", c.name)
		}
		for _, d := range ds {
			oRefused(t, "an id a never-served start-up recording's request holds, in the value's decoded form, asked a second before the call and "+c.name, d, RefusalUnknown, "mock-5")
		}
	}
}

// A view round F served that keploy cannot read may hold F's moment, so it
// counts as one that does (inAView, fail closed): an answer F served
// changed stays a source, and an answer whose view keploy cannot tell
// stays one it cannot tell, never one whose views hold nothing. Through
// Decide a view is read when it is handed in (Raw.viewOf), and one that
// cannot be read leaves the answer with no view at all, so this branch is
// pinned here, at the view.
func TestOutcomeAViewKeployCannotReadMayHoldFsMoment(t *testing.T) {
	answer := func(body string) *models.Mock {
		return getAnswer("mock-7", "http://api/jobs/1", body, eRecAt.Add(-time.Second))
	}
	var dr *DriftCache
	x := newReaderIndex(dr)
	dc := &decider{x: x, dr: dr}
	c := changedInstant{r: "82", l: "6c7d"}
	for _, v := range []struct {
		what string
		view Raw
		want bool
	}{
		{"a view that holds F's value", RawOf(answer(`{"job":"6c7d"}`), true), true},
		{"a view that holds another value", RawOf(answer(`{"job":"9a1e"}`), true), false},
		{"a view keploy cannot read", dr.LazyRawOf(answer(`{"job":"9a1e"}`), true, func() (*models.Mock, error) { return nil, errors.New("gone") }), true},
	} {
		p := evPlace{name: "mock-7", t: &placeText{views: []*readerText{x.textOf(v.view)}}}
		if got := dc.inAView(p, c); got != v.want {
			t.Errorf("%s: inAView %v; want %v", v.what, got, v.want)
		}
	}
}

// Whether a swap of start-ups could change a word at a place is asked only
// of a connection's recorded start-up: a recording that is none is no
// swap's, so the question loads nothing of it, for a word's evidence and an
// instant's pick alike, even past the set's store of payloads, where each
// read loads it again. A start-up is asked, and loaded to answer.
func TestOutcomeASwapQuestionLoadsNoRecordingThatIsNoStartUp(t *testing.T) {
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 0
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	for _, tc := range []struct {
		what    string
		m       *models.Mock
		startUp bool
	}{
		{"an answer", getAnswer("mock-7", "http://api/jobs/1", `{"pid":"82"}`, eRecAt.Add(-time.Second)), false},
		{"a connection's start-up", eSession("mock-hs-82", time.Second, 82), true},
	} {
		loads := 0
		raw := dr.LazyRawOf(tc.m, true, func() (*models.Mock, error) { loads++; return tc.m, nil })
		x := newReaderIndex(dr)
		p := evPlace{name: tc.m.Name, raw: 1, state: stateBefore, t: &placeText{rec: x.text(`{"pid":"82"}`), live: x.text(`{"pid":"83"}`)}}
		swapUnknown(Input{Raw: []Raw{raw}}, newWordPlaces([]evPlace{p}, "82"), readWord{Rec: "82", Live: "83"}, []evHeld{heldAt(p, "82", "83")})
		dc := &decider{in: Input{Raw: []Raw{raw}}, x: x, dr: dr}
		dc.swappable(p, changedInstant{r: "82", l: "83"})
		if tc.startUp != (loads > 0) {
			t.Errorf("%s: asked whether a swap could change its word, it was loaded %d times; want it loaded only where it is a start-up (%v)", tc.what, loads, tc.startUp)
		}
	}
}

// A decision lists the recordings it set aside because they were answered
// after the call (Decision.AnsweredAfter): each asked at or before the
// call's own instant and answered after it, whose answer holds the recorded
// word, however it is handed in, whatever round F gave there and whether or
// not round F served it at all. An adapter reads the list to refuse a word
// no replay would check. None other is listed: one asked and answered
// before the call (it decides), one answered at the call's own instant
// (unknown, refused), one asked after the call (after it by its request
// too), one whose answer holds no recorded word, and one whose request
// alone holds the word (placed by when it was sent). The verdicts are the
// ones the rows above pin.
func TestOutcomeADecisionListsTheRecordingsItSetAsideAsAnsweredAfterTheCall(t *testing.T) {
	ms := time.Millisecond
	placeOf := func(how string) []Place {
		if how == answeredAtHow[0] {
			return []Place{{Mock: "mock-6", Side: Response, Path: "row.0.id"}}
		}
		return []Place{{Mock: "mock-5", Side: Response}}
	}
	check := func(what string, d Decision, want []Place) {
		t.Helper()
		if !slices.Equal(d.AnsweredAfter, want) {
			t.Errorf("%s: set aside as answered after the call %+v; want %+v (%s)", what, d.AnsweredAfter, want, oDescribe([]Decision{d}))
		}
	}
	for _, c := range []struct {
		name     string
		req, res time.Time
		listed   bool
	}{
		{"asked and answered before the call", eRecAt.Add(-20 * ms), eRecAt.Add(-19 * ms), false},
		{"asked before the call and answered 10ms after it", eRecAt.Add(-10 * ms), eRecAt.Add(10 * ms), true},
		{"asked before the call and answered a second after it", eRecAt.Add(-10 * ms), eRecAt.Add(time.Second), true},
		{"asked at the call's own instant and answered after it", eRecAt, eRecAt.Add(ms), true},
		{"asked before the call and answered at its own instant", eRecAt.Add(-10 * ms), eRecAt, false},
		{"asked and answered at the call's own instant", eRecAt, eRecAt, false},
		{"asked and answered after the call", eRecAt.Add(time.Second), eRecAt.Add(time.Second + ms), false},
	} {
		for _, how := range answeredAtHow {
			for _, fView := range []bool{false, true} {
				var want []Place
				if c.listed {
					want = placeOf(how)
				}
				what := fmt.Sprintf("%s %s, round F's view holding the new value %v", how, c.name, fView)
				check(what, oWord(t, Decide(answeredAtInput(how, c.req, c.res, fView)), "82", "6c7d"), want)
				// A recorded word its answer does not hold is not set aside there.
				in := answeredAtInput(how, c.req, c.res, fView)
				in.Rec.Leaves["bind.$8"] = Leaf{Path: "bind.$8", Type: String, Value: "91"}
				check(what+", the recorded word another", oWord(t, Decide(in), "91", "6c7d"), nil)
			}
		}
	}
	// Never served: the recording on disk (an unserved pool read's shape).
	for _, c := range []struct {
		name   string
		before time.Duration
		res    time.Time
		listed bool
	}{
		{"asked before the call and answered after it", 10 * ms, eRecAt.Add(time.Second), true},
		{"asked at the call's own instant and answered after it", 0, eRecAt.Add(ms), true},
		{"asked and answered after the call", -time.Second, eRecAt.Add(time.Second + ms), false},
	} {
		in := answeredAtInput(answeredAtHow[1], eRecAt, eRecAt, false)
		in.Raw[len(in.Raw)-1] = ePgAnswered("mock-5", c.before, c.res, "SELECT id, n FROM jobs", []models.PostgresV3Cells{{{Value: "82"}, {Value: "5"}}}, "SELECT 1")
		var want []Place
		if c.listed {
			want = placeOf(answeredAtHow[1])
		}
		check("a recording round F never served, "+c.name, oWord(t, Decide(in), "82", "6c7d"), want)
	}
	// A new value sent at two places is one value, or two keploy cannot
	// tell apart: each place keeps what it set aside.
	for _, f2 := range []string{"6c7d", "7e8f"} {
		in := answeredAtInput(answeredAtHow[1], eRecAt.Add(-10*ms), eRecAt.Add(time.Second), false)
		in.Drift = append(in.Drift, "bind.$9")
		in.Rec.Leaves["bind.$9"] = Leaf{Path: "bind.$9", Type: String, Value: "82"}
		in.Live.Leaves["bind.$9"] = Leaf{Path: "bind.$9", Type: String, Value: f2}
		ds := Decide(in)
		if len(ds) != 2 {
			t.Fatalf("the value at two places, the second sent as %q: %s; want two decisions", f2, oDescribe(ds))
		}
		want := map[string][]WordOutcome{"6c7d": {OutcomeFresh, OutcomeValue}, "7e8f": {OutcomeRefused, OutcomeRefused}}[f2]
		for i, d := range ds {
			if d.Outcome != want[i] {
				t.Errorf("the value at two places, the second sent as %q: %s; want %v", f2, oDescribe(ds), want)
			}
			check("the value at two places, the second sent as "+f2, d, placeOf(answeredAtHow[1]))
		}
	}
	// Landed in another window, the word is refused and keeps the list.
	in := answeredAtInput(answeredAtHow[1], eRecAt.Add(-10*ms), eRecAt.Add(time.Second), false)
	in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedBetween}
	d := oWord(t, Decide(in), "82", "6c7d")
	if d.Refusal == nil || d.Refusal.Kind != RefusalLandedElsewhere {
		t.Errorf("landed between tests: %s; want refused as landed elsewhere", oDescribe([]Decision{d}))
	}
	check("landed between tests", d, placeOf(answeredAtHow[1]))
	// Another test's recording is read as a place and as an entry of the
	// set: listed once.
	in = answeredAtInput(answeredAtHow[1], eRecAt.Add(-10*ms), eRecAt.Add(time.Second), false)
	in.Raw[len(in.Raw)-1] = in.Raw[len(in.Raw)-1].OfAnotherTest()
	check("another test's recording", oWord(t, Decide(in), "82", "6c7d"), placeOf(answeredAtHow[1]))
	// A word the recording's request alone holds is placed by when it was
	// sent, so a late answer sets nothing aside.
	for _, how := range requestWordHow {
		check("a request word, "+how, oWord(t, Decide(requestWordInput(how, eRecAt.Add(-time.Second), eRecAt.Add(2*time.Second))), "82", "6c7d"), nil)
	}
}

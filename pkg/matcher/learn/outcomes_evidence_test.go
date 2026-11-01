package learn

import (
	"errors"
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
// instant, is unknown, refused naming why (chance_word_test.go:83).
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
		{"answered after the call", "before the call", answered(r1Insert.Add(time.Second))},
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
// (offset_test.go:2054). Without mock-44 holding round F's minute (it holds
// the recorded minute alone), mock-42 binds it. An answer that holds the
// minute only inside a time of it (11:14:45), which main binds by a cut, is
// no source a variable could hold: refused, naming it.
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
	for _, sent := range []struct{ what, v string }{
		{"15:51:05, cut to F's minute", base.Add(65 * time.Second).Format(time.RFC3339)},
		{"15:50:40, rounded to F's minute", base.Add(40 * time.Second).Format(time.RFC3339)},
		{"the minute 15:51 verbatim", base.Add(time.Minute).Format(minute)},
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
			oRefused(t, what+", served leaf by leaf", d, "unknown", "mock-44")
			d = oWord(t, Decide(call(nil, []Raw{RawOf(nb, true).ServedAs(1).OfAnotherTest()})), rec, live)
			oRefused(t, what+", handed as a recording", d, "unknown", "mock-44")
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

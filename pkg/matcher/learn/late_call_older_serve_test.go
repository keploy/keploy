package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A consumer that fell behind makes a call of an older test in a later
// test's window. mock-a answers 7, and record's full round gave it the
// application only in an older test than the call's (an adapter hands the
// previous test's serves as places of the call's own test, marked as
// Order.ServedView marks them: Raw.ServedInAnOlderTest, and
// Raw.ServedBeforeTheTest, which every older test's serve is too); mock-b,
// of the call's own test, answers 82. Both runs gave a, then b, before the
// call. The recording sent "slot 82", b's word: the application was on the
// window's test. Record's full round sent "slot 7", a's word: the
// application still worked on what the older test gave it. Each value is a
// copy of a place both runs gave, in the same order, so neither shows the
// other is no copy: the call is refused, naming the older serve, never
// learned as noise. So too where F served a changed and gave its view of
// it (the recording's a answered 5, or the view still gave 7 where the
// recording did and changed another row), where an adapter marks a serve
// of an older test alone (ServedInAnOlderTest: it need not wait for
// Order.ServedView), and where F gave a only before the call's test began
// in no older test (between tests, or at the set's start-up:
// Raw.ServedBeforeTheTest): the application may have kept what a gave it.
// And so where the recording made the late call ("slot 7") and F did not
// ("slot 82"), a's view in F giving 7 or another value (5): the order
// shows nothing either way.
//
// The controls keep the rule a copy follows its source: a in the call's own
// test, where the order alone decides (an application copying the last
// answer took b's 82 in both runs, so F's 7 is a's by chance); and a word of
// a serve before the test, in no older test, that the value holds by chance,
// where the recorded value holds no word of any place both runs gave (83):
// noise, as a word of another test's recording is let go on the same terms
// (excusedGiven). The same chance word of an older test's serve is refused,
// naming the serve, not as a late call: a place F gave the application only
// in an older test of the set excuses no word at all
// (TestAPlaceServedOnlyInAnOlderTestExcusesNoWord).
func TestALateCallsValueAnOlderServeGaveIsNeverLearnedAsNoise(t *testing.T) {
	const (
		late   = "record's full round gave the application mock-a only in an older test than the call's: the call may be a late call of that test"
		kept   = "record's full round gave the application mock-a only before the call's test began: the application may have kept what it gave there"
		noShow = "the recording cannot show the value does not follow that word"
	)
	older := func(x Raw) Raw { return x.ServedInAnOlderTest().ServedBeforeTheTest() }
	before := func(x Raw) Raw { return x.ServedBeforeTheTest() }
	b := eAnswer("mock-b", 200*time.Millisecond, "82").ServedAs(2).ServedAsRecorded()
	asRecorded := eAnswer("mock-a", 300*time.Millisecond, "7").ServedAs(1).ServedAsRecorded()
	viewed := eAnswer("mock-a", 300*time.Millisecond, "5").ServedAs(1).ServedLive(eAnswer("mock-a", 300*time.Millisecond, "7"))
	rows := func(second string) Raw {
		return ePg("mock-a", 300*time.Millisecond, "SELECT next_slot FROM mock-a", []models.PostgresV3Cells{{{Value: "7"}}, {{Value: second}}}, "SELECT 2")
	}
	heldView := rows("q1").ServedAs(1).ServedLive(rows("q2"))
	movedView := eAnswer("mock-a", 300*time.Millisecond, "7").ServedAs(1).ServedLive(eAnswer("mock-a", 300*time.Millisecond, "5"))
	for _, tc := range []struct {
		name           string
		recorded, live string
		a              Raw
		kind           Kind
		why            []string
		not            string
		// givenNoise: ClassifyGiven learns it as noise too.
		givenNoise bool
	}{
		{"a served as recorded in an older test", "slot 82", "slot 7", older(asRecorded), KindUnaccounted, []string{"stands in mock-b", late, noShow}, kept, false},
		{"a served as recorded in an older test, marked so alone", "slot 82", "slot 7", asRecorded.ServedInAnOlderTest(), KindUnaccounted, []string{"stands in mock-b", late, noShow}, kept, false},
		{"a served changed in an older test, F's view gave 7", "slot 82", "slot 7", older(viewed), KindUnaccounted, []string{"stands in mock-b", late, noShow}, kept, false},
		{"a served changed in an older test, F's view kept 7 where the recording gave it", "slot 82", "slot 7", older(heldView), KindUnaccounted, []string{"stands in mock-b", late, noShow}, kept, false},
		{"a served as recorded before the test, in no older test", "slot 82", "slot 7", before(asRecorded), KindUnaccounted, []string{"stands in mock-b", kept, noShow}, late, false},
		{"a served changed before the test, in no older test, F's view gave 7", "slot 82", "slot 7", before(viewed), KindUnaccounted, []string{"stands in mock-b", kept, noShow}, late, false},
		{"a served changed before the test, in no older test, F's view kept 7 where the recording gave it", "slot 82", "slot 7", before(heldView), KindUnaccounted, []string{"stands in mock-b", kept, noShow}, late, false},
		{"the recording's call late: a served as recorded in an older test", "slot 7", "slot 82", older(asRecorded), KindUnaccounted, []string{"stands in mock-b", late, noShow}, kept, false},
		{"the recording's call late: a served changed in an older test, F's view gave 5", "slot 7", "slot 82", older(movedView), KindUnaccounted, []string{"stands in mock-b", late, noShow}, kept, false},
		{"the recording's call late: a served as recorded before the test", "slot 7", "slot 82", before(asRecorded), KindUnaccounted, []string{"stands in mock-b", kept, noShow}, late, false},
		{"the recording's call late: a served changed before the test, F's view gave 5", "slot 7", "slot 82", before(movedView), KindUnaccounted, []string{"stands in mock-b", kept, noShow}, late, false},
		{"a served as recorded in the call's own test", "slot 82", "slot 7", asRecorded, KindNoise, nil, "", true},
		{"a served changed in the call's own test, F's view gave 7", "slot 82", "slot 7", viewed, KindNoise, nil, "", true},
		{"a served changed in the call's own test, F's view kept 7 where the recording gave it", "slot 82", "slot 7", heldView, KindNoise, nil, "", true},
		{"the recording's call took a's word: a served as recorded in the call's own test", "slot 7", "slot 82", asRecorded, KindNoise, nil, "", true},
		{"the recording's call took a's word: a served changed in the call's own test, F's view gave 5", "slot 7", "slot 82", movedView, KindNoise, nil, "", true},
		{"a served as recorded in an older test, the recorded value took no word", "slot 83", "slot 7", older(asRecorded), KindUnaccounted, []string{"in mock-a at", "record's full round gave the application mock-a " + olderSetAside}, late, false},
		{"a served changed in an older test, the recorded value took no word", "slot 83", "slot 7", older(viewed), KindUnaccounted, []string{"in mock-a at", "record's full round gave the application mock-a " + olderSetAside}, late, false},
		{"a served as recorded before the test, the recorded value took no word", "slot 83", "slot 7", before(asRecorded), KindNoise, nil, "", false},
		{"a served changed before the test, the recorded value took no word", "slot 83", "slot 7", before(viewed), KindNoise, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyF(t, tc.recorded, tc.live, nil, []Raw{tc.a, b}, nil)
			if got.Kind != tc.kind {
				t.Fatalf("%s at record, %s in F: %s (%q), want %s", tc.recorded, tc.live, got.Kind, got.Why, tc.kind)
			}
			for _, w := range tc.why {
				if !strings.Contains(got.Why, w) {
					t.Errorf("why %q; want it to say %q", got.Why, w)
				}
			}
			if tc.not != "" && strings.Contains(got.Why, tc.not) {
				t.Errorf("why %q says %q", got.Why, tc.not)
			}
			// The rule's absence (ClassifyGiven) refuses every one of
			// them the rule refuses: the older serve is a place of the
			// value. Where the search finds the value by a short part (82,
			// 7) at a place the rule sets aside, ClassifyGiven reads the
			// short parts under the rule (follows.short) and learns what
			// the rule learns: noise, in the call's own test. A value it
			// does not find so (83, no word of a place) stays refused.
			path := "bind.$8"
			rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.recorded}}}
			live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.live}}}
			if g := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, []Raw{tc.a, b}, nil, nil, nil); len(g) != 1 || (g[0].Kind == KindNoise) != tc.givenNoise || g[0].Kind != KindNoise && g[0].Kind != KindUnaccounted && !g[0].From.SetAside {
				t.Errorf("ClassifyGiven: %+v, want noise %v (else refused)", g, tc.givenNoise)
			}
		})
	}
}

// The late call's value may come from an exchange a learn step served leaf
// by leaf (Earlier) as well as from a payload (Raw). mock-s is the set's
// start-up exchange, a login whose request carried a value the run made, so
// the run keeps it for every later test (Earlier.Run, StartUp); its answer
// gave 7 in both runs. Read by a later test's call, the read's order shows
// F gave it only before the call's test began (InServeOrder marks it:
// Earlier.beforeTest). mock-b, of the call's own test, answers 82. As with
// a payload, neither value shows the other is no copy, in either
// direction: refused, naming the start-up exchange. So too of the place an
// earlier test's call left the run (Earlier.Run, not StartUp: its request
// leaves, here one that held 7 in both runs), which the application sent in
// an older test. Where F's leaf moved (F gave 5 where the recording gave 7),
// F's value, which holds no 5, is no copy of F's leaf, but the recording's
// call may still have kept the 7 its run gave there: with the recording's
// call the late one, the start-up exchange and the earlier test's place are
// refused too, as each is where F's leaf stayed.
// The controls: the same exchange read by a call made at the start-up,
// which no order marks (no test of the call's had begun), where the order
// alone decides; the moved exchange of the call's own test, which the copy
// rule lets go; and a word of it the value holds by chance (the recorded
// value holds no word of a place both runs gave: 83), let go.
func TestALateCallsValueAnExchangeServedBeforeItsTestGaveIsNeverLearnedAsNoise(t *testing.T) {
	const (
		kept   = "record's full round gave the application mock-s only before the call's test began: the application may have kept what it gave there"
		sent   = "record's full round's application sent mock-r's request only in an older test than the call's: the call may be a late call of that test"
		noShow = "the recording cannot show the value does not follow that word"
	)
	b := eAnswer("mock-b", 200*time.Millisecond, "82").ServedAs(2).ServedAsRecorded()
	startUp := Earlier{Mock: "mock-s", At: eRecAt.Add(-300 * time.Millisecond), Run: true, StartUp: true, beforeTest: true,
		Payload: eAnswer("mock-s", 300*time.Millisecond, "7").ServedAs(1),
		Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "body.token", Type: String, Value: "7"}, Live: "7"},
			{Side: Request, Leaf: Leaf{Path: "body.nonce", Type: String, Value: "n1"}, Live: "n2", Place: "body.nonce"},
		}}
	atStartUp := startUp
	atStartUp.beforeTest = false
	ran := Earlier{Mock: "mock-r", At: eRecAt.Add(-300 * time.Millisecond), Run: true,
		Payload: Raw{mock: "mock-r", at: eRecAt.Add(-300 * time.Millisecond), answered: eRecAt.Add(-299 * time.Millisecond), ordered: true}.ServedAs(1),
		Seen:    []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Type: String, Value: "7 n1"}, Live: "7 n2", Place: "body.slot"}}}
	// F gave 5 where the recording gave 7: at the start-up exchange's answer,
	// and at the earlier test's call's request.
	moved := startUp
	moved.Seen = []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.token", Type: String, Value: "7"}, Live: "5"},
		startUp.Seen[1],
	}
	movedOwn := moved
	movedOwn.Run, movedOwn.StartUp, movedOwn.beforeTest = false, false, false
	ranMoved := ran
	ranMoved.Seen = []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Type: String, Value: "7 n1"}, Live: "5 n2", Place: "body.slot"}}
	for _, tc := range []struct {
		name           string
		recorded, live string
		e              Earlier
		kind           Kind
		why            string
	}{
		{"the start-up exchange, read in a later test", "slot 82", "slot 7", startUp, KindUnaccounted, kept},
		{"the start-up exchange, read in a later test, the recording's call late", "slot 7", "slot 82", startUp, KindUnaccounted, kept},
		{"an older test's call's request", "slot 82", "slot 7", ran, KindUnaccounted, sent},
		{"an older test's call's request, the recording's call late", "slot 7", "slot 82", ran, KindUnaccounted, sent},
		{"the start-up exchange, its answer moved in F, the recording's call late", "slot 7", "slot 82", moved, KindUnaccounted, kept},
		{"an older test's call's request, moved in F, the recording's call late", "slot 7", "slot 82", ranMoved, KindUnaccounted, sent},
		{"the call's own test's exchange, its answer moved in F, the recording's call late", "slot 7", "slot 82", movedOwn, KindNoise, ""},
		{"the start-up exchange, read at the start-up", "slot 82", "slot 7", atStartUp, KindNoise, ""},
		{"the start-up exchange, read at the start-up, the recording's call took its word", "slot 7", "slot 82", atStartUp, KindNoise, ""},
		{"the start-up exchange, read in a later test, the recorded value took no word", "slot 83", "slot 7", startUp, KindNoise, ""},
		{"an older test's call's request, the recorded value took no word", "slot 83", "slot 7", ran, KindNoise, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyF(t, tc.recorded, tc.live, []Earlier{tc.e}, []Raw{b}, nil)
			if got.Kind != tc.kind {
				t.Fatalf("%s at record, %s in F: %s (%q), want %s", tc.recorded, tc.live, got.Kind, got.Why, tc.kind)
			}
			if tc.why == "" {
				return
			}
			for _, w := range []string{"stands in mock-b", tc.why, noShow} {
				if !strings.Contains(got.Why, w) {
					t.Errorf("why %q; want it to say %q", got.Why, w)
				}
			}
		})
	}
}

// InServeOrder marks an exchange F served only before the call's test began
// (Earlier.beforeTest) as Order.ServedView marks a Raw of its recording: the
// set's start-up exchange of the run, read by a call of a later test. Read
// by a call made at the start-up (no test had begun), or once F served its
// recording again in the call's own test, it is no such exchange, and stays
// unmarked: the order then decides (it lists the recording at its first and
// its last serve). An order of no tracker marks nothing.
func TestInServeOrderMarksAnExchangeServedOnlyBeforeTheTest(t *testing.T) {
	now := time.Now()
	login := &models.Mock{Name: "mock-s", Kind: models.HTTP, Spec: models.MockSpec{
		ReqTimestampMock: now.Add(-time.Minute),
		ResTimestampMock: now.Add(-time.Minute + time.Millisecond),
		HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://api.local/login"},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"token":"7"}`},
	}}
	tr := NewTracker()
	tr.NewSet()
	tr.ServedMock(login)
	n, _, _, _ := tr.Test()
	tr.Served(n, Earlier{Mock: login.Name, At: login.Spec.ReqTimestampMock, Payload: RawOf(login, true),
		Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "body.token", Type: String, Value: "7"}, Live: "7"},
			{Side: Request, Leaf: Leaf{Path: "header.X-Nonce", Type: String, Value: "n1"}, Live: "n2", Place: "header.X-Nonce"},
		}})
	tr.ServedMockAsRecorded(login)
	read := func() Earlier {
		t.Helper()
		p := tr.Placed()
		for _, e := range InServeOrder(p.Earlier, append(append([]*models.Mock(nil), p.Served...), p.Before...), p.Order) {
			if e.Mock == login.Name && e.Run && e.StartUp {
				return e
			}
		}
		t.Fatalf("no run's start-up exchange of %s in %+v", login.Name, p.Earlier)
		return Earlier{}
	}
	if e := read(); e.beforeTest || e.outsideTest() {
		t.Errorf("read at the start-up: beforeTest %v, outsideTest %v; want neither", e.beforeTest, e.outsideTest())
	}
	tr.Begin()
	tr.End()
	tr.Begin()
	if e := read(); !e.beforeTest || !e.outsideTest() {
		t.Errorf("read in a later test: beforeTest %v, outsideTest %v; want both", e.beforeTest, e.outsideTest())
	}
	// An order of no tracker (Placed of none) tells nothing: nothing is
	// marked.
	p := tr.Placed()
	for _, e := range InServeOrder(p.Earlier, []*models.Mock{login}, Order{}) {
		if e.beforeTest {
			t.Errorf("an order of no tracker marked %s", e.Mock)
		}
	}
	tr.ServedMock(login)
	tr.ServedMockAsRecorded(login)
	if e := read(); e.beforeTest || e.outsideTest() {
		t.Errorf("served again in the call's own test: beforeTest %v, outsideTest %v; want neither", e.beforeTest, e.outsideTest())
	}
}

// A serve the fix reads as made before the call's test is one Order.ServedView
// marks so. A serve of the late-call shape it leaves unmarked, read as a
// place of the call's own test where the order decides, is refused by the
// order: F made it before the test and again in the call's own test (the
// full round's order lists it at its first and its last serve, which the
// recording's, one place each, never matches), or keploy cannot tell where
// F made it (ServedIn(0, 0): unordered beside b). The control is a served
// once, before b, in the call's own test: noise, as the copy rule says.
func TestALateCallsValueAServeLeftUnmarkedGaveIsRefusedByTheOrder(t *testing.T) {
	b := func(n int) Raw { return eAnswer("mock-b", 200*time.Millisecond, "82").ServedAs(n).ServedAsRecorded() }
	a := func(first, last int) Raw {
		return eAnswer("mock-a", 300*time.Millisecond, "7").ServedIn(first, last).ServedAsRecorded()
	}
	for _, tc := range []struct {
		name string
		raw  []Raw
		kind Kind
	}{
		{"a served at 1 and 3, b at 2", []Raw{a(1, 3), b(2)}, KindUnaccounted},
		{"a served at 1 and 2, b at 3", []Raw{a(1, 2), b(3)}, KindUnaccounted},
		{"a served in an order keploy cannot tell, b at 2", []Raw{a(0, 0), b(2)}, KindUnaccounted},
		{"a served once at 1, b at 2", []Raw{a(1, 1), b(2)}, KindNoise},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyF(t, "slot 82", "slot 7", nil, tc.raw, nil); got.Kind != tc.kind {
				t.Fatalf("slot 82 at record, slot 7 in F: %s (%q), want %s", got.Kind, got.Why, tc.kind)
			}
		})
	}
}

// In keploy mock's client tests (mockDrifts) the word rule's re-read lets a
// chance piece of a minted id go (follows.idOnly), and a late call is read
// once. The late call's UUIDs, minted in each run: the recorded id's 16-bit
// group stands by chance in mock-60, a recording of the call's own test that
// F gave as recorded (inside another UUID), and the id the call carries now
// an older test's serve gave the application whole (mock-22, test 2). The
// call is refused as a late call of that test, never learned as a minted id:
// F's id stands whole where F gave it. The control, where the older serve
// gave another id: no late call.
func TestALateCallOfAMintedIDStaysRefusedInKeployMock(t *testing.T) {
	const (
		path  = "body.Key.Id.S"
		late  = "late call of mock-22's test"
		piece = "the value is an id minted in each run"
	)
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateRecID}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateLiveID}}}
	asRecorded := func(x Raw) Raw { return x.ServedAsRecorded() }
	window := lateMsg("mock-60", 200*time.Millisecond, "c0ffee11-5d1e-4a1b-8c2d-0123456789ab").ServedAs(9).ServedAsRecorded()
	given := func(older string) Learned {
		t.Helper()
		set := []GivenPlaces{lateSet("mock-22", 2, older, asRecorded)}
		got := ClassifyGiven(mockDrifts(), []string{path}, live, rec, eRecAt, nil, append(eStartup(), window), nil, nil, func() ([]GivenPlaces, error) { return set, nil })
		if len(got) != 1 {
			t.Fatalf("%+v", got)
		}
		return got[0]
	}
	if got := given(lateLiveID); got.Kind != KindUnaccounted || !strings.Contains(got.Why, late) || strings.Contains(got.Why, piece) {
		t.Errorf("FALSE GREEN? F's id whole in an older test's serve: %s from %q (%q); want refused as a late call of mock-22's test", got.Kind, got.From.Mock, got.Why)
	}
	if got := given(lateOtherID); strings.Contains(got.Why, late) {
		t.Errorf("the older serve gave another id: %s from %q (%q); want no late call", got.Kind, got.From.Mock, got.Why)
	} else {
		t.Logf("control: %s (%q)", got.Kind, got.Why)
	}
}

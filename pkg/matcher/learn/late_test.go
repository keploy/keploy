package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The late call's ids: the one the call's recording holds (in a recording
// of its own test, lateWindowRec), and the one an older test's serve gave
// the application in record's full round, which the late call carries.
const (
	lateRecID  = "fb77a0c2-5d1e-4c3b-9a8f-0e6d2c4b1a90"
	lateLiveID = "7f7e49f4-2b6c-4e1d-8f3a-5c9b0d7e6a12"
	// lateOtherID is an id neither the call nor its recording holds.
	lateOtherID = "0a0b0c0d-1e1f-4a4b-8c8d-9e9f0a0b0c0d"
)

// lateMsg is a recording whose answer holds id, recorded before eRecAt.
func lateMsg(name string, before time.Duration, id string) Raw {
	return ePg(name, before, "SELECT id FROM lead_events", []models.PostgresV3Cells{{{Value: id}}}, "SELECT 1")
}

// lateSet is a recording F gave the application in the set's test n (0: one
// keploy cannot name), as the word rule reads the set's (GivenPlaces).
func lateSet(name string, n int, id string, serve func(Raw) Raw) GivenPlaces {
	g := PlacesOf(serve(lateMsg(name, 40*time.Second, id).ServedAs(3)))
	g.words.test = n
	return g
}

// A consumer that fell behind makes a call of an older test in a later
// test's window: it carries the id an older test's serve gave it (mock-22,
// test 2), while the call's recording holds the id of a recording of the
// window's own test (mock-60). The search for the recorded value's source
// finds mock-60, and the reason said the value "flowed from mock-60, where
// keploy cannot follow it yet": the wrong recording, so the user could not
// act on it. The reason names the late call and the serve that gave the
// value, never mock-60 as its source; where no older test's serve it can
// name gave the value, and F gave mock-60 without it, that keploy cannot
// name where it came from. The verdict stays a refusal. Where keploy cannot
// tell what F gave at mock-60, or F gave the value there, the reason is the
// search's, as before, and so where F never served mock-60, whether an
// older serve gave the value or not (the reason says F never served it).
func TestALateCallNamesTheOlderTestsServeNotTheWindowsRecording(t *testing.T) {
	const path = "body.Key.Id.S"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateRecID}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateLiveID}}}
	asRecorded := func(x Raw) Raw { return x.ServedAsRecorded() }
	const (
		late    = "late call of mock-22's test"
		search  = "flowed from mock-60"
		unnamed = "keploy cannot name where the value the call carries now came from"
	)
	for _, c := range []struct {
		name   string
		window Raw
		set    []GivenPlaces
		want   string
		not    []string
		// fol is what the reason says under the rule a copy follows its
		// source (ClassifyFollowing), where it sets the window's recording
		// aside as no source (F gave it as recorded, and the value moved):
		// the word rule then names the older serve; "" is want.
		fol string
	}{
		{"an older test's serve gave it", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, late, []string{search, "follows it as a value from there", "a part of the value"}, "appears in mock-22"},
		{"the window's recording not served yet", lateMsg("mock-60", 200*time.Millisecond, lateRecID),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, "never served before this call", []string{late, unnamed}, ""},
		{"two older tests' serves gave it", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded), lateSet("mock-33", 3, lateLiveID, asRecorded)}, "cannot tell which of them it carries", []string{search}, "appears in mock-22"},
		{"F gave it at the window's recording", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedLive(lateMsg("mock-60", 200*time.Millisecond, lateLiveID)),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, search, []string{late}, ""},
		{"no adapter said what F gave at the window's recording", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, search, []string{late}, ""},
		{"the older serve's test keploy cannot name", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 0, lateLiveID, asRecorded)}, unnamed, []string{search, late}, "appears in mock-22"},
		{"no adapter said what F gave at the older serve", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, func(x Raw) Raw { return x })}, unnamed, []string{search, late}, "appears in mock-22"},
		{"the older serve holds another id", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateOtherID, asRecorded)}, unnamed, []string{search, late}, "-"},
		{"F's view of the window's recording kept its id", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedLive(lateMsg("mock-60", 200*time.Millisecond, lateRecID)),
			[]GivenPlaces{lateSet("mock-22", 2, lateOtherID, asRecorded)}, "gave the application that word as recorded", []string{search, late}, "-"},
		{"F gave its view of the older serve", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateOtherID, func(x Raw) Raw { return x.ServedLive(lateMsg("mock-22", 40*time.Second, lateLiveID)) })}, late, []string{search, unnamed}, "-"},
		{"F gave the older serve changed, with a view keploy could not read", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, func(x Raw) Raw { return x.ServedChanged() })}, unnamed, []string{search, late}, "-"},
		{"F's serves of the older serve gave views that differ", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, func(x Raw) Raw {
				return x.ServedChanged(lateMsg("mock-22", 40*time.Second, lateLiveID), lateMsg("mock-22", 40*time.Second, lateOtherID))
			})}, unnamed, []string{search, late}, "-"},
		{"a recording F gave earlier in the set could not be read", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded), {}}, unnamed, []string{search, late}, "-"},
		{"four older tests' serves gave it", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded), lateSet("mock-33", 3, lateLiveID, asRecorded), lateSet("mock-44", 4, lateLiveID, asRecorded), lateSet("mock-55", 5, lateLiveID, asRecorded)},
			"in each of mock-22, mock-33, mock-44 and 1 more", []string{search, "mock-55"}, "appears in mock-22"},
		{"a part of the recorded id stands in the window's recording", lateMsg("mock-60", 200*time.Millisecond, lateRecID[:8]).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, "a part of the value it was recorded with (its characters 1 to 8) stands in mock-60", []string{search, "stands whole"}, "appears in mock-22"},
		{"the window's recording not served yet, and no older serve gave it", lateMsg("mock-60", 200*time.Millisecond, lateRecID),
			[]GivenPlaces{lateSet("mock-22", 2, lateOtherID, asRecorded)}, "never served before this call", []string{late, unnamed}, ""},
	} {
		for _, cl := range []struct {
			name string
			fol  bool
			run  func(raw []Raw, given GivenEarlier) []Learned
		}{
			{"ClassifyGiven", false, func(raw []Raw, given GivenEarlier) []Learned {
				return ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, raw, nil, nil, given)
			}},
			{"ClassifyFollowing", true, func(raw []Raw, given GivenEarlier) []Learned {
				return ClassifyFollowing(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, raw, nil, nil, given)
			}},
		} {
			at := c.name + ", " + cl.name
			want := c.want
			if cl.fol && c.fol == "-" {
				// A value no source accounts for, varying under that
				// rule: no reason of this row's.
				continue
			}
			if cl.fol && c.fol != "" {
				want = c.fol
			}
			set := c.set
			got := cl.run(append(eStartup(), c.window), func() ([]GivenPlaces, error) { return set, nil })
			if len(got) != 1 {
				t.Fatalf("%s: %d learned, want 1", at, len(got))
			}
			ln := got[0]
			// The verdict: refused, as before (a variable of a recording's
			// payload, which no protocol binds, or unaccounted).
			if ln.Kind != KindVariable && ln.Kind != KindUnaccounted {
				t.Errorf("%s: %s (%q); want refused", at, ln.Kind, ln.Why)
			}
			if ln.Kind == KindVariable && ln.From.Side != "" {
				t.Errorf("%s: a variable bound at %s %s; want refused", at, ln.From.Mock, ln.From.Side)
			}
			if !strings.Contains(ln.Why, want) {
				t.Errorf("%s: why %q; want it to say %q", at, ln.Why, want)
			}
			// A late call's leaf is no variable of any place; one whose
			// source keploy cannot name stays a variable of the window's
			// recording, set aside (Place.SetAside), and no other is.
			if ln.Kind == KindUnaccounted && ln.From != (Place{}) {
				t.Errorf("%s: unaccounted from %+v; want no place", at, ln.From)
			}
			if aside := strings.Contains(ln.Why, unnamed); ln.From.SetAside != aside || aside && (ln.Kind != KindVariable || ln.From.Mock != "mock-60") {
				t.Errorf("%s: %s from %q set aside %v; want set aside %v", at, ln.Kind, ln.From.Mock, ln.From.SetAside, aside)
			}
			for _, n := range c.not {
				if strings.Contains(ln.Why, n) {
					t.Errorf("%s: why %q says %q", at, ln.Why, n)
				}
			}
		}
	}
}

// A value every word of which that changed is a read of the clock that
// moved by the lead (clockSet.whole) is left to the clock's rules: an
// answer's instant is followed whole whatever F gave there, so the reason
// stays the search's even where F gave the window's recording as recorded
// and no older test's serve gave the value.
func TestALateCallReasonLeavesAReadOfTheClockToItsRules(t *testing.T) {
	const path = "body.at"
	recMs := eRecAt.Add(-200 * time.Millisecond).UnixMilli()
	recV, liveV := strconv.FormatInt(recMs, 10), strconv.FormatInt(recMs+FullRoundLead.Milliseconds(), 10)
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: recV}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: liveV}}}
	window := lateMsg("mock-60", 200*time.Millisecond, recV).ServedAs(9).ServedAsRecorded()
	set := []GivenPlaces{lateSet("mock-22", 2, lateOtherID, func(x Raw) Raw { return x.ServedAsRecorded() })}
	got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, append(eStartup(), window), nil, nil, func() ([]GivenPlaces, error) { return set, nil })
	if len(got) != 1 {
		t.Fatalf("%d learned, want 1", len(got))
	}
	if ln := got[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-60" || strings.Contains(ln.Why, "cannot name") {
		t.Errorf("%s from %q (%q); want the search's variable of mock-60", ln.Kind, ln.From.Mock, ln.Why)
	}
}

// The serves of a mock name the test of the set F made them in (testOf),
// read from the tracker's order: an older test's alone, never the start-up,
// the gap between two tests (a serve after a test's last one too), two
// tests' (one the next test's first serve), or the running test's, each
// numbered in its own set, after an earlier set's tests.
func TestOrderNamesTheOlderTestOfAServe(t *testing.T) {
	m := func(name string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: eRecAt.Add(-time.Minute),
			ResTimestampMock: eRecAt.Add(-time.Minute + time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: "SELECT id FROM lead_events",
				Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: lateLiveID}}}, CommandComplete: "SELECT 1"},
			}},
		}}
	}
	tr := NewTracker()
	// An earlier set's tests, which a new set forgets: its tests are
	// numbered from its own first.
	tr.NewSet()
	tr.Begin()
	for i := 0; i < 10; i++ {
		tr.ServedMock(m("an earlier set's " + strconv.Itoa(i)))
	}
	tr.End()
	tr.Begin()
	tr.ServedMock(m("an earlier set's last"))
	tr.End()
	tr.NewSet()
	tr.ServedMock(m("startup"))
	tr.Begin()
	tr.ServedMock(m("one"))
	tr.ServedMock(m("both"))
	tr.ServedMock(m("then between"))
	tr.ServedMock(m("then the next test's first"))
	tr.End()
	tr.ServedMock(m("between"))
	tr.ServedMock(m("then between"))
	tr.Begin()
	tr.ServedMock(m("then the next test's first"))
	tr.ServedMock(m("two"))
	tr.ServedMock(m("two"))
	tr.ServedMock(m("both"))
	tr.End()
	tr.Begin()
	tr.ServedMock(m("three"))
	o := tr.Placed().Order
	for name, want := range map[string]int{"startup": 0, "one": 1, "between": 0, "then between": 0, "then the next test's first": 0, "two": 2, "both": 0, "three": 0, "never": 0} {
		o.t.mu.Lock()
		first, last := o.ofLocked(MockID(m(name)))
		got := o.testOf(first, last)
		o.t.mu.Unlock()
		if got != want {
			t.Errorf("%s (serves %d to %d): test %d, want %d", name, first, last, got, want)
		}
	}
	// The word rule reads the set's recordings so named (GivenPlaces),
	// and a recording handed to the call as F served it (ServedView), the
	// test F ran just before the running one (two's) said so (prev).
	for name, want := range map[string]int{"startup": 0, "one": 1, "two": 2, "both": 0, "three": 0} {
		g, err := o.GivenPlaces(m(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if g.words.test != want || g.words.prev != (want == 2) {
			t.Errorf("%s as the word rule reads it: test %d previous %v, want %d", name, g.words.test, g.words.prev, want)
		}
		r := o.ServedView(m(name), RawOf(m(name), true).ServedIn(o.Of(m(name))))
		if r.test != want || r.prev != (want == 2) {
			t.Errorf("%s as F served it: test %d previous %v, want %d", name, r.test, r.prev, want)
		}
		if r = r.WithServed(false); r.test != 0 || r.prev {
			t.Errorf("%s unserved: test %d previous %v, want none", name, r.test, r.prev)
		}
	}
	// In a set's first test, a serve of the start-up is of no test, and
	// none the test before the running one.
	tr.NewSet()
	tr.ServedMock(m("startup"))
	tr.Begin()
	o = tr.Placed().Order
	if g, err := o.GivenPlaces(m("startup")); err != nil || g.words.test != 0 || g.words.prev {
		t.Errorf("a start-up's serve in the set's first test: test %d previous %v (%v), want none", g.words.test, g.words.prev, err)
	}
}

// The late call's reason names what the search found at the window's
// recording as it found it: where the call's recorded value is an encoded
// form (a '5' written %35) and the window's recording holds it decoded, it
// stands there whole, as the search's own reason says (evidence), never as
// a part of it.
func TestALateCallsReasonNamesTheDecodedRecordedValueWhole(t *testing.T) {
	const path = "body.Key.Id.S"
	const (
		decoded = "fb77a0c25d1e4c3b9a8f0e6d2c4b1a90"
		encoded = "fb77a0c2%35d1e4c3b9a8f0e6d2c4b1a90"
		liveID  = "7f7e49f42b6c4e1d8f3a5c9b0d7e6a12"
	)
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: encoded}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: liveID}}}
	window := lateMsg("mock-60", 200*time.Millisecond, decoded).ServedAs(9).ServedAsRecorded()
	set := []GivenPlaces{lateSet("mock-22", 2, liveID, func(x Raw) Raw { return x.ServedAsRecorded() })}
	got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, append(eStartup(), window), nil, nil, func() ([]GivenPlaces, error) { return set, nil })
	if len(got) != 1 {
		t.Fatalf("%d learned, want 1", len(got))
	}
	ln := got[0]
	if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "late call of mock-22's test") || !strings.Contains(ln.Why, "the value it was recorded with stands whole in mock-60") || strings.Contains(ln.Why, "a part of") {
		t.Errorf("%s (%q); want refused as a late call of mock-22's test, the recorded value whole in mock-60", ln.Kind, ln.Why)
	}
}

// The late call's reason is built only at a place the search found in a
// recording's payload, by what F gave there (the rule a copy follows its
// source reads its words). A value the call's own test sent earlier, at a
// leaf of a request whose recording F answered as recorded (mock-60), keeps
// its verdict, a variable bound at that leaf, and an
// instant found in another layout (an echo: F's application was served the
// moment it carries, a timetable's row) keeps the search's reason: what F
// gave there holds the moment F saw, in no text the copy rule reads.
func TestALateCallsReasonLeavesASeenLeafAndAnEchoToTheSearch(t *testing.T) {
	const path = "body.Key.Id.S"
	set := []GivenPlaces{lateSet("mock-22", 2, lateLiveID, func(x Raw) Raw { return x.ServedAsRecorded() })}
	given := func() ([]GivenPlaces, error) { return set, nil }
	t.Run("a leaf the test sent earlier", func(t *testing.T) {
		rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateRecID}}}
		live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateLiveID}}}
		window := lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded()
		seen := Earlier{Mock: "mock-60", At: eRecAt.Add(-200 * time.Millisecond), Payload: window, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.id", Type: String, Value: lateRecID}, Live: lateLiveID},
		}}
		got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, []Earlier{seen}, append(eStartup(), window), nil, nil, given)
		if len(got) != 1 {
			t.Fatalf("%d learned, want 1", len(got))
		}
		if ln := got[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-60" || ln.From.Side != Request || strings.Contains(ln.Why, "late call") || strings.Contains(ln.Why, "cannot name") {
			t.Errorf("%s from %q %q (%q); want the variable bound at mock-60's request leaf, as before", ln.Kind, ln.From.Mock, ln.From.Side, ln.Why)
		}
	})
	t.Run("an echo", func(t *testing.T) {
		recAt := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
		liveAt := recAt.Add(72 * time.Hour)
		rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: recAt.Format(time.RFC3339)}}}
		live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: liveAt.Format(time.RFC3339)}}}
		ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
		timetable := ePg("mock-60", 200*time.Millisecond, "SELECT departs FROM timetable", []models.PostgresV3Cells{{{Value: ms(recAt)}}, {{Value: ms(liveAt)}}}, "SELECT 2").ServedAs(9).ServedAsRecorded()
		got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, append(eStartup(), timetable), nil, nil, given)
		if len(got) != 1 {
			t.Fatalf("%d learned, want 1", len(got))
		}
		if ln := got[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-60" || !ln.From.Echo || strings.Contains(ln.Why, "cannot name") || strings.Contains(ln.Why, "late call") {
			t.Errorf("%s from %q echo %v (%q); want the search's echo of mock-60, as before", ln.Kind, ln.From.Mock, ln.From.Echo, ln.Why)
		}
	})
}

// An older test's serve never names a late call by a word of its stamps
// (an HTTP-date its answer holds: a day, a month, digits, a time, "GMT",
// which any value may equal by chance): no such word is an id (lateID),
// which alone names one. A call that now carries "Sat", which only the Date
// of mock-22's answer holds, is no late call of mock-22's test: keploy
// cannot name where the value came from, and the reason says so.
func TestALateCallsReasonNamesNoServeByAWordOfItsDateStamp(t *testing.T) {
	const path = "body.day"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "Wed"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "Sat"}}}
	window := lateMsg("mock-60", 200*time.Millisecond, "Wed").ServedAs(9).ServedAsRecorded()
	asked := eRecAt.Add(-40 * time.Second)
	dated := RawOf(&models.Mock{Name: "mock-22", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "HTTP_CLIENT"},
		HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/status"},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": "Sat, 24 Oct 2026 09:55:13 GMT"}, Body: `{"ok":true}`},
		ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Millisecond),
	}}, false)
	g := PlacesOf(dated.ServedAs(3).ServedAsRecorded())
	g.words.test = 2
	set := []GivenPlaces{g}
	got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, append(eStartup(), window), nil, nil, func() ([]GivenPlaces, error) { return set, nil })
	if len(got) != 1 {
		t.Fatalf("%d learned, want 1", len(got))
	}
	if ln := got[0]; ln.Kind != KindVariable || !strings.Contains(ln.Why, "keploy cannot name where the value the call carries now came from") || strings.Contains(ln.Why, "mock-22") || strings.Contains(ln.Why, "late call") {
		t.Errorf("%s (%q); want refused, saying keploy cannot name the value's source, naming no serve of mock-22", ln.Kind, ln.Why)
	}
}

// A late call is named only by a value no answer holds by chance (lateID):
// an id. A count, a flag, a status or a code an older test's serve holds
// shows nothing of where the call's value came from, so where F gave the
// window's recording as recorded the reason says keploy cannot name the
// source, never that the call is late; an id the older serve gave whole
// names it.
func TestALateCallIsNamedOnlyByAnID(t *testing.T) {
	const path = "body.n"
	for _, c := range []struct {
		rec, live string
		late      bool
	}{
		{"3", "4", false},
		{"false", "true", false},
		{"active", "pending", false},
		{"200", "201", false},
		{"20261103", "20261104", false},
		{"ord-7K29Q3AB", "ord-7K29Q4AB", true},
		{lateRecID, lateLiveID, true},
	} {
		rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: c.rec}}}
		live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: c.live}}}
		window := lateMsg("mock-60", 200*time.Millisecond, c.rec).ServedAs(9).ServedAsRecorded()
		set := []GivenPlaces{lateSet("mock-22", 2, c.live, func(x Raw) Raw { return x.ServedAsRecorded() })}
		got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, append(eStartup(), window), nil, nil, func() ([]GivenPlaces, error) { return set, nil })
		if len(got) != 1 {
			t.Fatalf("%q to %q: %d learned, want 1", c.rec, c.live, len(got))
		}
		ln := got[0]
		if ln.Kind != KindVariable && ln.Kind != KindUnaccounted || ln.Kind == KindVariable && ln.From.Side != "" {
			t.Errorf("%q to %q: %s (%q); want refused", c.rec, c.live, ln.Kind, ln.Why)
		}
		named := strings.Contains(ln.Why, "late call of mock-22's test")
		unnamed := strings.Contains(ln.Why, "keploy cannot name where the value the call carries now came from")
		if named != c.late || unnamed == c.late || !c.late && strings.Contains(ln.Why, "mock-22") {
			t.Errorf("%q to %q: why %q; want the late call named %v", c.rec, c.live, ln.Why, c.late)
		}
	}
	for s, want := range map[string]bool{"a1b2c3d4": true, "A1B2C3D4": true, "a1b2c3d": false, "abcdefgh": false, "12345678": false, "ab-12_cd": true, "ab 12 cdef": false, "ab12cd.e": false} {
		if got := lateID(s); got != want {
			t.Errorf("lateID(%q) = %v, want %v", s, got, want)
		}
	}
}

// Where the value moved as a word of one stamp to the same word of another
// (stampsEchoed: the recorded "Wed" the day of mock-60's recorded Date, the
// live "Sat" the day of a Date F served), the stamps are searched as any
// text, and so where the late-call reason asks what F gave the application
// at the window's recording. Where F gave mock-60 as recorded and mock-59
// gave "Sat", mock-60's Date holds no "Sat", and the reason says keploy
// cannot name where the value came from. Where mock-60's own Last-Modified,
// which F gave as recorded, holds "Sat", or F's view of mock-60 gave its
// Date as "Sat", F gave the value there, and the reason is the search's.
func TestALateCallsReasonReadsTheWindowsStampsWhereTheyEcho(t *testing.T) {
	const path = "body.day"
	const unnamed = "keploy cannot name where the value the call carries now came from"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "Wed"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "Sat"}}}
	dated := func(name string, before time.Duration, date string, more ...string) Raw {
		asked := eRecAt.Add(-before)
		header := map[string]string{"Date": date}
		if len(more) > 0 {
			header["Last-Modified"] = more[0]
		}
		return RawOf(&models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/status/" + name},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: header, Body: `{"ok":true}`},
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Millisecond),
		}}, false)
	}
	const wed, sat = "Wed, 21 Oct 2026 09:55:13 GMT", "Sat, 24 Oct 2026 09:55:13 GMT"
	for _, c := range []struct {
		name  string
		raw   []Raw
		aside bool
	}{
		{"F gave the window's recording as recorded", []Raw{dated("mock-59", 300*time.Millisecond, sat).ServedAs(8).ServedAsRecorded(), dated("mock-60", 200*time.Millisecond, wed).ServedAs(9).ServedAsRecorded()}, true},
		{"the window's recording, given as recorded, holds Sat in another stamp", []Raw{dated("mock-60", 200*time.Millisecond, wed, sat).ServedAs(9).ServedAsRecorded()}, false},
		{"F's view of the window's recording gave its Date as Sat", []Raw{dated("mock-60", 200*time.Millisecond, wed).ServedAs(9).ServedLive(dated("mock-60", 200*time.Millisecond, sat))}, false},
	} {
		got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, append(eStartup(), c.raw...), nil, nil, func() ([]GivenPlaces, error) { return nil, nil })
		if len(got) != 1 {
			t.Fatalf("%s: %d learned, want 1", c.name, len(got))
		}
		ln := got[0]
		if ln.Kind != KindVariable || ln.From.Mock != "mock-60" || ln.From.SetAside != c.aside || strings.Contains(ln.Why, unnamed) != c.aside {
			t.Errorf("%s: %s from %q set aside %v (%q); want a variable of mock-60, set aside %v", c.name, ln.Kind, ln.From.Mock, ln.From.SetAside, ln.Why, c.aside)
		}
	}
	// Real stamps (each Date its answer's own instant, AnswerStamp): the
	// value moved from "55", the minute of mock-60's Date, to "53", the
	// minute of mock-57's, which F served (stampsEchoed). mock-60's own
	// Date, which F gave as recorded, holds "53" too (its second): F gave
	// the value there, and the reason is the search's, as the stamps are
	// searched as any text there too.
	minRec := Shape{Leaves: map[string]Leaf{"body.min": {Path: "body.min", Type: String, Value: "55"}}}
	minLive := Shape{Leaves: map[string]Leaf{"body.min": {Path: "body.min", Type: String, Value: "53"}}}
	raw := []Raw{
		dated("mock-57", 2*time.Minute, "Sat, 24 Oct 2026 09:53:53 GMT").ServedAs(8).ServedAsRecorded(),
		dated("mock-60", 200*time.Millisecond, "Sat, 24 Oct 2026 09:55:53 GMT").ServedAs(9).ServedAsRecorded(),
	}
	got := ClassifyGiven(clockedDrifts(), []string{"body.min"}, minLive, minRec, eRecAt, nil, append(eStartup(), raw...), nil, nil, func() ([]GivenPlaces, error) { return nil, nil })
	if len(got) != 1 || got[0].Kind != KindVariable || got[0].From.Mock != "mock-60" || got[0].From.SetAside || strings.Contains(got[0].Why, unnamed) {
		t.Errorf("a minute of the window's own Date that holds the value F saw: %+v; want a variable of mock-60, not set aside", got)
	}
}

// A consumer one test behind: the late call carries the id a serve of the
// test F ran just before the call's gave the application (mock-22). That
// recording is handed to the call (Tracker.Before: an earlier place of the
// call's own test, which Order.ServedView marks as served in one older
// test), not read among the set's; the reason names it as the serve of the
// test F ran just before the call's, and says why keploy follows no value
// of it here, as it names a serve of a test two back (an older test's), or
// both. A recording F served in two older tests names no test: keploy then
// names no serve, and says it cannot name the source. The protocols that
// read a recording handed to the call so (HTTP, Postgres) classify by
// ClassifyGiven; Mongo's rule (ClassifyFollowing) sets the window's
// recording aside before this reason is read, and is not this row's.
func TestALateCallNamesThePreviousTestsServeHandedToTheCall(t *testing.T) {
	const path = "body.Key.Id.S"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateRecID}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateLiveID}}}
	const (
		late     = "late call of mock-22's test"
		prev     = "in mock-22, in the test record's full round ran just before the call's"
		older    = "in mock-22, in an older test than the call's"
		notPrev  = "the call was recorded with another value than the application got there"
		notOlder = "keploy does not follow a value another test's recording gave the application"
		unnamed  = "keploy cannot name where the value the call carries now came from"
	)
	window := lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded()
	// served is a recording F served in older test n of the set, as
	// ServedView marks it (n 0: in two older tests), previous where n is
	// the test F ran just before the call's.
	served := func(x Raw, n int, previous bool) Raw {
		x = x.ServedAs(3).ServedAsRecorded().ServedInAnOlderTest()
		x.test, x.prev = n, previous
		return x
	}
	for _, c := range []struct {
		name string
		raw  []Raw
		set  []GivenPlaces
		want []string
		not  []string
	}{
		{"the previous test's serve gave it", []Raw{served(lateMsg("mock-22", 40*time.Second, lateLiveID), 3, true)}, nil,
			[]string{late, prev, notPrev}, []string{older, notOlder, unnamed, "flowed from mock-60"}},
		{"a serve two tests back gave it", []Raw{served(lateMsg("mock-22", 40*time.Second, lateLiveID), 2, false)}, nil,
			[]string{late, older, notOlder}, []string{prev, notPrev, unnamed}},
		{"the previous test's serve and an older one's gave it", []Raw{served(lateMsg("mock-22", 40*time.Second, lateLiveID), 3, true)},
			[]GivenPlaces{lateSet("mock-11", 1, lateLiveID, func(x Raw) Raw { return x.ServedAsRecorded() })},
			[]string{"in each of mock-22, mock-11", notPrev}, []string{notOlder, unnamed}},
		{"the previous test's serve holds another id", []Raw{served(lateMsg("mock-22", 40*time.Second, lateOtherID), 3, true)}, nil,
			[]string{unnamed}, []string{late, "mock-22"}},
		{"a serve made in two older tests gave it", []Raw{served(lateMsg("mock-22", 40*time.Second, lateLiveID), 0, false)}, nil,
			[]string{unnamed}, []string{late, "mock-22"}},
	} {
		at := c.name
		set := c.set
		got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, append(append(eStartup(), c.raw...), window), nil, nil, func() ([]GivenPlaces, error) { return set, nil })
		if len(got) != 1 {
			t.Fatalf("%s: %d learned, want 1", at, len(got))
		}
		ln := got[0]
		// Refused, as before: a late call's leaf is no variable of any
		// place, and one whose source keploy cannot name stays a
		// variable of the window's recording, set aside.
		switch named := strings.Contains(ln.Why, late) || strings.Contains(ln.Why, "in each of"); {
		case named && (ln.Kind != KindUnaccounted || ln.From != (Place{})):
			t.Errorf("%s: %s from %+v; want unaccounted, from no place", at, ln.Kind, ln.From)
		case !named && (ln.Kind != KindVariable || ln.From.Mock != "mock-60" || ln.From.Side != "" || !ln.From.SetAside):
			t.Errorf("%s: %s from %+v; want a variable of mock-60, set aside", at, ln.Kind, ln.From)
		}
		for _, w := range c.want {
			if !strings.Contains(ln.Why, w) {
				t.Errorf("%s: why %q; want it to say %q", at, ln.Why, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(ln.Why, n) {
				t.Errorf("%s: why %q says %q", at, ln.Why, n)
			}
		}
	}
}

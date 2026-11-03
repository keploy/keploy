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
		late    = "late call of test 2"
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
			"mock-44 (the set's test 4) and 1 more", []string{search, "mock-55"}, "appears in mock-22"},
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
	// The word rule reads the set's recordings so named (GivenPlaces).
	for name, want := range map[string]int{"one": 1, "two": 2, "three": 0} {
		g, err := o.GivenPlaces(m(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if g.words.test != want {
			t.Errorf("%s as the word rule reads it: test %d, want %d", name, g.words.test, want)
		}
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
	if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "late call of test 2") || !strings.Contains(ln.Why, "the value it was recorded with stands whole in mock-60") || strings.Contains(ln.Why, "a part of") {
		t.Errorf("%s (%q); want refused as a late call of test 2, the recorded value whole in mock-60", ln.Kind, ln.Why)
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

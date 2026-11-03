package learn

import (
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
// value, never mock-60 as its source; the verdict stays a refusal. Where
// keploy cannot tell what F gave at either place, or F gave the value at
// mock-60, the reason is the search's, as before.
func TestALateCallNamesTheOlderTestsServeNotTheWindowsRecording(t *testing.T) {
	const path = "body.Key.Id.S"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateRecID}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: lateLiveID}}}
	asRecorded := func(x Raw) Raw { return x.ServedAsRecorded() }
	const (
		late   = "late call of test 2"
		search = "flowed from mock-60"
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
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, late, []string{search, "follows it as a value from there"}, "appears in mock-22"},
		{"the window's recording not served yet", lateMsg("mock-60", 200*time.Millisecond, lateRecID),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, late, []string{search}, ""},
		{"two older tests' serves gave it", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded), lateSet("mock-33", 3, lateLiveID, asRecorded)}, "cannot tell which of them it carries", []string{search}, "appears in mock-22"},
		{"F gave it at the window's recording", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedLive(lateMsg("mock-60", 200*time.Millisecond, lateLiveID)),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, search, []string{late}, ""},
		{"no adapter said what F gave at the window's recording", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, asRecorded)}, search, []string{late}, ""},
		{"the older serve's test keploy cannot name", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 0, lateLiveID, asRecorded)}, search, []string{late}, "appears in mock-22"},
		{"no adapter said what F gave at the older serve", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, lateLiveID, func(x Raw) Raw { return x })}, search, []string{late}, "appears in mock-22"},
		{"the older serve holds another id", lateMsg("mock-60", 200*time.Millisecond, lateRecID).ServedAs(9).ServedAsRecorded(),
			[]GivenPlaces{lateSet("mock-22", 2, "0a0b0c0d-1e1f-4a4b-8c8d-9e9f0a0b0c0d", asRecorded)}, search, []string{late}, "-"},
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

// The serves of a mock name the test of the set F made them in (testOf),
// read from the tracker's order: an older test's alone, never the start-up,
// the gap between two tests, two tests', or the running test's.
func TestOrderNamesTheOlderTestOfAServe(t *testing.T) {
	m := func(name string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{ReqTimestampMock: eRecAt.Add(-time.Minute)}}
	}
	tr := NewTracker()
	tr.NewSet()
	tr.ServedMock(m("startup"))
	tr.Begin()
	tr.ServedMock(m("one"))
	tr.ServedMock(m("both"))
	tr.End()
	tr.ServedMock(m("between"))
	tr.Begin()
	tr.ServedMock(m("two"))
	tr.ServedMock(m("two"))
	tr.ServedMock(m("both"))
	tr.End()
	tr.Begin()
	tr.ServedMock(m("three"))
	o := tr.Placed().Order
	for name, want := range map[string]int{"startup": 0, "one": 1, "between": 0, "two": 2, "both": 0, "three": 0, "never": 0} {
		o.t.mu.Lock()
		first, last := o.ofLocked(MockID(m(name)))
		got := o.testOf(first, last)
		o.t.mu.Unlock()
		if got != want {
			t.Errorf("%s (serves %d to %d): test %d, want %d", name, first, last, got, want)
		}
	}
}

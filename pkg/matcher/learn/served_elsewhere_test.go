package learn

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording F served that no earlier place of the call's own test is
// (another test's, or one recorded after the call: Raw.OfAnotherTest) is
// read by the word rule as it reads a place of the call's own test where F
// may have given the application a word the recording did not: a word the
// value holds now that F's view of it holds (Raw.ServedLive), or a view of
// one of its serves where they differ (Raw.ServedChanged), and every such
// word where keploy cannot tell what F gave there (no view stated:
// Raw.viewUnstated; a view it could not read), is refused, naming the
// recording. It was read as the recording's answer alone, so a value the
// application copied from what F gave in another test, or one F may have
// given there, was learned as noise and never compared again (FALSE GREEN),
// whatever order F served it in. Served as recorded, not served, or with
// views that hold no word of the value, it learns, as before. So under
// ClassifyGiven and ClassifyFollowing, read whole or lazily, with a cache of
// no tracker and with a set's, and the same verdicts as of a place of the
// call's own test.
func TestAWordAnotherTestsRecordingFServedMayHaveGivenIsRefused(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const (
		learns    = ""
		unstated  = "may stand in mock-6, a recording record's full round gave the application before the call that is another test's"
		ownUnst   = "may stand in mock-6, an earlier recording record's full round served, where its adapter did not say what it gave the application there"
		unread    = "served changed, where keploy could not read what it gave the application"
		ownUnread = "where keploy could not read what it gave the application"
		gave      = "appears in mock-6 as record's full round gave it, where the recording did not give the application that word, a recording record's full round gave the application before the call that is another test's"
		inAView   = "serves that gave the application different views of it, where the recording did not give the application that word"
		ownGave   = "as record's full round gave it"
	)
	for _, c := range []struct {
		name       string
		serve      func(x Raw) Raw
		other, own string
	}{
		{"served, no view stated", func(x Raw) Raw { return x.ServedAs(6) }, unstated, ownUnst},
		{"served in an order keploy cannot tell, no view stated", func(x Raw) Raw { return x.ServedIn(0, 0) }, unstated, ownUnst},
		{"served as recorded", func(x Raw) Raw { return x.ServedAs(6).ServedAsRecorded() }, learns, learns},
		{"not served", func(x Raw) Raw { return x.WithServed(false) }, learns, learns},
		{"served with a view that holds the value's word", func(x Raw) Raw { return x.ServedAs(6).ServedLive(eJobs("6c7d", "9")) }, gave, ownGave},
		{"served with a view that holds no word of the value", func(x Raw) Raw { return x.ServedAs(6).ServedLive(eJobs(chanceSrcRec, "9")) }, learns, learns},
		{"served changed, a view it could not read", func(x Raw) Raw { return x.ServedAs(6).ServedChanged() }, unread, ownUnread},
		{"served changed, one of two views holds the value's word", func(x Raw) Raw {
			return x.ServedAs(6).ServedChanged(eJobs(chanceSrcRec, "9"), eJobs("6c7d", "9"))
		}, inAView, inAView},
		{"served changed, no view holds a word of the value", func(x Raw) Raw {
			return x.ServedAs(6).ServedChanged(eJobs(chanceSrcRec, "9"), eJobs(chanceSrcRec, "7"))
		}, learns, learns},
	} {
		for _, cl := range []struct {
			name string
			run  func(dc *DriftCache, raw []Raw) []Learned
		}{
			{"ClassifyGiven", func(dc *DriftCache, raw []Raw) []Learned {
				return ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
			}},
			{"ClassifyFollowing", func(dc *DriftCache, raw []Raw) []Learned {
				return ClassifyFollowing(dc, []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
			}},
		} {
			for _, lazy := range []bool{false, true} {
				for _, set := range []bool{false, true} {
					for _, other := range []bool{true, false} {
						at := c.name + ", " + cl.name
						if lazy {
							at += ", lazy"
						}
						if set {
							at += ", a set's cache"
						}
						want := c.own
						if other {
							at += ", another test's"
							want = c.other
						}
						dc := clockedDrifts()
						if !set {
							dc = noSet(dc)
						}
						x := eJobs(chanceSrcRec, "5")
						if lazy {
							x = dc.LazyRawOf(jobsMock(chanceSrcRec, "5"), false, nil)
						}
						x = c.serve(x)
						if other {
							x = x.OfAnotherTest()
						}
						got := cl.run(dc, append(eStartup(), x))[0]
						switch {
						case want == learns && got.Kind == KindUnaccounted:
							t.Errorf("%s: refused (%q); want learned: no word of the value stands in what F gave there", at, got.Why)
						case want != learns && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, want)):
							t.Errorf("FALSE GREEN? %s: %s (%q); want refused: %q", at, got.Kind, got.Why, want)
						}
					}
				}
			}
		}
	}
}

// An exchange F served leaf by leaf whose request was recorded after the
// call (leafedAfterCall: no earlier place of the call's own test, read as
// what F gave the application before the call) whose adapter did not say F
// served it as recorded: what F gave the application there is a view no
// adapter stated (Earlier.lateOf), so a word the value holds now that may
// stand there is refused, as lateOf says it is. It was learned as noise.
// Said as recorded, the value learns.
func TestAWordALateExchangeFServedWithNoViewStatedMayHaveGivenIsRefused(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	m := jobsMock(chanceSrcRec, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt.Add(time.Second), eRecAt.Add(time.Second+time.Millisecond)
	for _, asRecorded := range []bool{false, true} {
		e := Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, AsRecorded: asRecorded, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}}
		for _, cl := range []struct {
			name string
			run  func() []Learned
		}{
			{"ClassifyGiven", func() []Learned {
				return ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, []Earlier{e}, eStartup(), nil, nil, nil)
			}},
			{"ClassifyFollowing", func() []Learned {
				return ClassifyFollowing(clockedDrifts(), []string{path}, live, rec, eRecAt, []Earlier{e}, eStartup(), nil, nil, nil)
			}},
		} {
			got := cl.run()[0]
			if asRecorded {
				if got.Kind == KindUnaccounted {
					t.Errorf("%s, said as recorded: refused (%q); want learned", cl.name, got.Why)
				}
				continue
			}
			const want = "may stand in mock-6, a recording record's full round gave the application before the call that was recorded after the call"
			if got.Kind != KindUnaccounted || !strings.Contains(got.Why, want) {
				t.Errorf("FALSE GREEN? %s, said nothing of: %s (%q); want refused: %q", cl.name, got.Kind, got.Why, want)
			}
		}
	}
}

// F's order of a serve made while two of the application's requests were
// aloft on parallel connections (a pooled driver's), in no request of its
// own, is one keploy cannot tell (Order.Of: 0, 0). A recording served so,
// that no adapter stated a view of, is read as a view no adapter stated
// (Order.ServedView keeps it so), and a word a value holds now that may
// stand there is refused, whether it is a place of the call's own test or
// another test's: the word rule reads no serve order. Stated with a view
// that holds no word of the value, it learns. (A case mongo-vars' mutant M3
// was thought to show learned as noise; it is not.)
func TestAWordARecordingServedInAnOrderKeployCannotTellWithNoViewStatedIsRefused(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	m := jobsMock(chanceSrcRec, "5")
	for _, stated := range []bool{false, true} {
		tr := NewTracker()
		tr.Begin()
		a, b := tr.Lift(), tr.Lift()
		tr.ServedMockAtIn(m, time.Now(), nil)
		if stated {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return jobsMock(chanceSrcRec, "9"), nil })
		}
		a.Land()
		b.Land()
		o := tr.Placed().Order
		first, last := o.Of(m)
		if first != 0 || last != 0 {
			t.Fatalf("a serve in no request beside two aloft is at %d..%d; want an order keploy cannot tell (0, 0)", first, last)
		}
		for _, other := range []bool{false, true} {
			x := o.ServedView(m, RawOf(m, true).ServedIn(first, last))
			if x.viewUnstated() == stated {
				t.Fatalf("stated %v: the serve reads as unstated %v", stated, x.viewUnstated())
			}
			if other {
				x = x.OfAnotherTest()
			}
			for _, cl := range []struct {
				name string
				run  func(raw []Raw) []Learned
			}{
				{"ClassifyGiven", func(raw []Raw) []Learned {
					return ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
				}},
				{"ClassifyFollowing", func(raw []Raw) []Learned {
					return ClassifyFollowing(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
				}},
			} {
				got := cl.run(append(eStartup(), x))[0]
				switch {
				case stated && got.Kind == KindUnaccounted:
					t.Errorf("%s, another test's %v, a view stated: refused (%q); want learned", cl.name, other, got.Why)
				case !stated && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, "where its adapter did not say what it gave the application there")):
					t.Errorf("FALSE GREEN? %s, another test's %v, no view stated: %s (%q); want refused", cl.name, other, got.Kind, got.Why)
				}
			}
		}
	}
}

// An exchange F served leaf by leaf, read as a recording F gave the
// application before the call that is no earlier place of the call's own
// test (Earlier.lateOf), whose adapter gave F's view of it (Earlier.View: the
// application sent another request, F gave it the recorded answer) is read
// with that view, as a later call reads the recording (Order.ServedView): a
// word a value holds now that only the view holds is refused, naming the
// recording as record's full round gave it; a view that cannot be read
// refuses every word; one that holds no word of the value learns. lateOf
// read the view as one no adapter stated, so a fresh value beside any HTTP
// exchange served at the start-up with another request was refused (a false
// red), and the reason never named the word's place.
func TestALateExchangeIsReadWithTheViewItsAdapterGave(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	m := jobsMock(chanceSrcRec, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt.Add(time.Second), eRecAt.Add(time.Second+time.Millisecond)
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) {
			v := jobsMock(id, "9")
			v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
			return v, nil
		}
	}
	const (
		learns = ""
		gave   = "appears in mock-6 as record's full round gave it, where the recording did not give the application that word, a recording record's full round gave the application before the call that was recorded after the call"
		unread = "served changed, where keploy could not read what it gave the application"
	)
	for _, c := range []struct {
		name string
		view func() (*models.Mock, error)
		want string
	}{
		{"a view that holds the value's word", view("6c7d"), gave},
		{"a view that holds no word of the value", view(chanceSrcRec), learns},
		{"a view that cannot be read", func() (*models.Mock, error) { return nil, errors.New("gone") }, unread},
	} {
		e := Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Payload: RawOf(m, true), View: c.view, ViewKey: "k",
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}}
		for _, cl := range []struct {
			name string
			run  func() []Learned
		}{
			{"ClassifyGiven", func() []Learned {
				return ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, []Earlier{e}, eStartup(), nil, nil, nil)
			}},
			{"ClassifyFollowing", func() []Learned {
				return ClassifyFollowing(clockedDrifts(), []string{path}, live, rec, eRecAt, []Earlier{e}, eStartup(), nil, nil, nil)
			}},
		} {
			got := cl.run()[0]
			switch {
			case c.want == learns && got.Kind == KindUnaccounted:
				t.Errorf("%s, %s: refused (%q); want learned: no word of the value stands in what F gave there", cl.name, c.name, got.Why)
			case c.want != learns && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, c.want)):
				t.Errorf("FALSE GREEN? %s, %s: %s (%q); want refused: %q", cl.name, c.name, got.Kind, got.Why, c.want)
			}
		}
	}
}

// A recording F served in an older test of the running set, read among the
// recordings F gave the application earlier in the set (Order.GivenPlaces,
// GivenEarlier: the place an HTTP or Mongo call reads it at), is read with
// what its adapters stated of the serves of it the read saw, as a Raw is
// (Order.ServedView): told as recorded, a fresh value learns; told with a
// view that holds the value's word, or with two views one of which does
// (the recording's own among them where a serve was told as recorded), it
// is refused naming the recording and how many views F's serves gave; told
// with a view that cannot be read, or told of nothing, every word is
// refused. Each view's words are read once for the set, by its place among
// the recording's views (setPlaces.views), however many calls read it. The set read every recording
// as its recorded answer alone, so a value the application copied from what
// F gave it two tests back (a template value, a variable F rendered), or
// one F may have given there, was learned as noise (FALSE GREEN).
func TestARecordingAnOlderTestServedIsReadWithWhatItsAdaptersStated(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	m := jobsMock(chanceSrcRec, "5")
	// loads counts the reads of each view's loader: the set reads a view's
	// words once (setPlaces.views), however many calls read the recording.
	loads := map[string]int{}
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) { loads[id]++; return jobsMock(id, "9"), nil }
	}
	const (
		learns   = ""
		unstated = "may stand in mock-6, a recording record's full round gave the application before the call that is another test's"
		unread   = "served changed, where keploy could not read what it gave the application"
		gave     = "appears in mock-6 as record's full round gave it, where the recording did not give the application that word"
		inTwo    = "in one of 2 serves that gave the application different views of it, where the recording did not give the application that word"
	)
	for _, c := range []struct {
		name  string
		tell  func(tr *Tracker)
		want  string
		views int // different views read, each once for the set
	}{
		{"told of nothing", func(*Tracker) {}, unstated, 0},
		{"told as recorded", func(tr *Tracker) { tr.ServedMockAsRecorded(m) }, learns, 0},
		{"told with a view that holds the value's word", func(tr *Tracker) { tr.ServedMockLive(m, "a", view("6c7d")) }, gave, 1},
		{"told with a view that holds no word of the value", func(tr *Tracker) { tr.ServedMockLive(m, "a", view(chanceSrcRec)) }, learns, 1},
		{"told with a view that cannot be read", func(tr *Tracker) {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return nil, errors.New("gone") })
		}, unread, 0},
		{"told with a view that cannot be kept", func(tr *Tracker) { tr.ServedMockLive(m, "", nil) }, unread, 0},
		{"told as recorded, then with a view that holds the value's word", func(tr *Tracker) {
			tr.ServedMockAsRecorded(m)
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", view("6c7d"))
		}, inTwo, 1},
		// Each view is the set's by its place among the recording's views:
		// the second's words are its own, never the first's.
		{"told with two views, the second holding the value's word", func(tr *Tracker) {
			tr.ServedMockLive(m, "a", view(chanceSrcRec))
			tr.ServedMock(m)
			tr.ServedMockLive(m, "b", view("6c7d"))
		}, inTwo, 2},
	} {
		clear(loads)
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		c.tell(tr)
		tr.End()
		tr.Begin()
		tr.End()
		tr.Begin()
		o := tr.Placed().Order
		for _, cl := range []struct {
			name string
			run  func(given GivenEarlier) []Learned
		}{
			{"ClassifyGiven", func(given GivenEarlier) []Learned {
				return ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, eStartup(), nil, nil, given)
			}},
			{"ClassifyFollowing", func(given GivenEarlier) []Learned {
				return ClassifyFollowing(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, eStartup(), nil, nil, given)
			}},
		} {
			// Twice: the second read takes the set's places and views from
			// its cache.
			for _, read := range []string{"first read", "cached read"} {
				got := cl.run(func() ([]GivenPlaces, error) {
					g, err := o.GivenPlaces(m)
					return []GivenPlaces{g}, err
				})[0]
				at := c.name + ", " + cl.name + ", " + read
				switch {
				case c.want == learns && got.Kind == KindUnaccounted:
					t.Errorf("%s: refused (%q); want learned: no word of the value stands in what F gave there", at, got.Why)
				case c.want != learns && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, c.want)):
					t.Errorf("FALSE GREEN? %s: %s (%q); want refused: %q", at, got.Kind, got.Why, c.want)
				}
			}
		}
		read := 0
		for id, n := range loads {
			read++
			if n != 1 {
				t.Errorf("%s: the view of %s was read %d times over four reads of one set; want once", c.name, id, n)
			}
		}
		if read != c.views {
			t.Errorf("%s: %d views read; want %d", c.name, read, c.views)
		}
	}
}

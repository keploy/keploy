package learn

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording F served is read with the view its adapters stated of its
// serves (Order.ServedView): as served as recorded only where every serve
// of it the call's read saw was told so (Tracker.ServedMockAsRecorded, or
// Served of an exchange its adapter said F served as recorded, every Seen
// bearing it out). A value that holds no word of it then learns, as a
// recording served as recorded does. A serve no adapter told so (the mock
// manager's alone), one serve more than statements, a statement made before
// its serve or of the previous set, or an exchange its adapter said nothing
// of, said as recorded while a Seen shows another value, or with no payload
// read from its mock, leaves F's view unstated: the value's new word is
// refused, naming the recording (fail closed: a false red, never a word F
// may have given there passed as noise). A read keeps the view it saw: a
// later serve no adapter told so changes the next read, not it.
func TestOrderServedViewReadsARecordingAsRecordedOnlyWhereEveryServeWasToldSo(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const unstated = "may stand in mock-6, an earlier recording record's full round served, where its adapter did not say what it gave the application there"
	m := jobsMock(chanceSrcRec, "5")
	exchange := func(asRecorded bool, served string, payload bool) Earlier {
		e := Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, AsRecorded: asRecorded,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: served}}}
		if payload {
			e.Payload = noSet(clockedDrifts()).LazyRawOf(m, true, nil)
		}
		return e
	}
	served := func(tr *Tracker, e Earlier) {
		tr.ServedMock(m)
		tr.Served(tr.Epoch(), e)
	}
	for _, c := range []struct {
		name string
		run  func(tr *Tracker)
		want bool // read as served as recorded
	}{
		{"served once, told so", func(tr *Tracker) { tr.ServedMock(m); tr.ServedMockAsRecorded(m) }, true},
		{"served once, told by the mock manager alone", func(tr *Tracker) { tr.ServedMock(m) }, false},
		{"served twice, told once", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockAsRecorded(m)
			tr.ServedMock(m)
		}, false},
		{"served twice, told twice", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockAsRecorded(m)
			tr.ServedMock(m)
			tr.ServedMockAsRecorded(m)
		}, true},
		{"told before its serve", func(tr *Tracker) { tr.ServedMockAsRecorded(m); tr.ServedMock(m) }, false},
		{"told in the previous set", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockAsRecorded(m)
			tr.NewSet()
			tr.Begin()
			tr.ServedMock(m)
		}, false},
		{"an exchange served as recorded", func(tr *Tracker) { served(tr, exchange(true, "5", true)) }, true},
		{"an exchange served as recorded in an earlier test", func(tr *Tracker) {
			served(tr, exchange(true, "5", true))
			tr.End()
			tr.Begin()
		}, true},
		{"an exchange of the previous set told in this one", func(tr *Tracker) {
			n := tr.Epoch()
			tr.NewSet()
			tr.Begin()
			tr.ServedMock(m)
			tr.Served(n, exchange(true, "5", true))
		}, false},
		{"an exchange its adapter says nothing of", func(tr *Tracker) { served(tr, exchange(false, "5", true)) }, false},
		{"an exchange said as recorded whose Seen shows another value", func(tr *Tracker) { served(tr, exchange(true, "6", true)) }, false},
		{"an exchange said as recorded with no payload", func(tr *Tracker) { served(tr, exchange(true, "5", false)) }, false},
	} {
		tr := NewTracker()
		tr.Begin()
		c.run(tr)
		p := tr.Placed()
		dc := noSet(clockedDrifts())
		first, last := p.Order.Of(m)
		r := p.Order.ServedView(m, dc.LazyRawOf(m, true, nil).ServedIn(first, last))
		if r.AsRecorded() != c.want || r.viewUnstated() == c.want {
			t.Errorf("%s: read as recorded %v, unstated %v; want as recorded %v", c.name, r.AsRecorded(), r.viewUnstated(), c.want)
			continue
		}
		got := ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, append(eStartup(), r), nil, nil, nil)
		switch {
		case c.want && got[0].Kind == KindUnaccounted:
			t.Errorf("%s: refused (%q); want learned: no word of it stands in what F gave", c.name, got[0].Why)
		case !c.want && (got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, unstated)):
			t.Errorf("FALSE GREEN? %s: %s (%q); want refused: %q", c.name, got[0].Kind, got[0].Why, unstated)
		}
	}

	// A read keeps the view it saw; the next read sees the serve no adapter
	// told so.
	tr := NewTracker()
	tr.Begin()
	tr.ServedMock(m)
	tr.ServedMockAsRecorded(m)
	before := tr.Placed().Order
	tr.ServedMock(m)
	after := tr.Placed().Order
	r := noSet(clockedDrifts()).LazyRawOf(m, true, nil).ServedAs(1)
	if !before.ServedView(m, r).AsRecorded() || after.ServedView(m, r).AsRecorded() {
		t.Errorf("a serve no adapter told so after a read: the read reads as recorded %v, the next %v; want true, false", before.ServedView(m, r).AsRecorded(), after.ServedView(m, r).AsRecorded())
	}
	// A view already stated, a Raw F did not serve, and one of another mock
	// are returned as they are.
	tr = NewTracker()
	tr.Begin()
	tr.ServedMock(m)
	tr.ServedMockAsRecorded(m)
	o := tr.Placed().Order
	if v := o.ServedView(m, r.ServedChanged()); v.AsRecorded() {
		t.Errorf("a view keploy could not read read as recorded")
	}
	if v := o.ServedView(m, r.WithServed(false)); v.AsRecorded() || v.Served() {
		t.Errorf("a recording F did not serve read as served")
	}
	other := &models.Mock{Name: "mock-7", Kind: m.Kind, Spec: m.Spec}
	if v := o.ServedView(other, r); v.AsRecorded() {
		t.Errorf("another mock's statements read for mock-6")
	}
}

// An exchange F served leaf by leaf, read as a recording F gave the
// application before the call that is no earlier place of the call's own
// test (lateOf), is read as served as recorded, its payload and what F
// served at its answer's leaves, only where its adapter said F served it so
// and every Seen bears it out; otherwise both are views no adapter stated,
// refused where they may have given a word (fail closed).
func TestALateExchangeIsReadAsRecordedOnlyWhereItsAdapterSaidSo(t *testing.T) {
	m := jobsMock(chanceSrcRec, "5")
	for _, c := range []struct {
		name       string
		asRecorded bool
		served     string
		want       bool
	}{
		{"said as recorded", true, "5", true},
		{"said nothing of", false, "5", false},
		{"said as recorded, a Seen shows another value", true, "6", false},
	} {
		e := Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, AsRecorded: c.asRecorded, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: c.served}}}
		out := e.lateOf(true)
		if len(out) != 2 {
			t.Fatalf("%s: %d Raws; want the payload and what F served at its answer's leaves", c.name, len(out))
		}
		for i, x := range out {
			if x.AsRecorded() != c.want || x.viewUnstated() == c.want {
				t.Errorf("%s, Raw %d: as recorded %v, unstated %v; want as recorded %v", c.name, i, x.AsRecorded(), x.viewUnstated(), c.want)
			}
		}
	}
}

// An exchange F served leaf by leaf whose adapter did not say there how F
// served it (Mongo's learn step: it tells the serve of the recording once
// the answer is encoded, after it reported the exchange, by
// Tracker.ServedMockAsRecorded or Tracker.ServedMockLive) is read, as a
// recording F gave the application before the call (lateOf), with what its
// adapter told of the recording's serves the read saw (InServeOrder,
// Order.ServedView): as recorded, or with the view F gave, never as one
// whose view no adapter stated, which refuses every word a value holds now
// that may stand there (a false red on every Mongo call that reads a
// recording made after it). A serve of it told of nothing, a statement of
// another recording of its name and request time, or an exchange said as
// recorded that a leaf shows F served otherwise, leaves it unstated (fail
// closed).
func TestALateExchangeIsReadWithWhatItsAdapterToldOfTheServeAfterIt(t *testing.T) {
	m := jobsMock(chanceSrcRec, "5")
	view := jobsMock(chanceSrcRec, "6")
	other := jobsMock(chanceSrcRec, "5")
	other.Spec.ResTimestampMock = other.Spec.ResTimestampMock.Add(time.Millisecond)
	exchange := func(asRecorded bool, served string) Earlier {
		return Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, AsRecorded: asRecorded, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: served}}}
	}
	for _, c := range []struct {
		name           string
		e              Earlier
		mocks          []*models.Mock
		before         func(tr *Tracker) // serves before the exchange's
		after          func(tr *Tracker) // statements after it
		recorded, live bool
	}{
		{name: "told as recorded", e: exchange(false, "5"), after: func(tr *Tracker) { tr.ServedMockAsRecorded(m) }, recorded: true},
		{name: "told with a view", e: exchange(false, "5"), after: func(tr *Tracker) {
			tr.ServedMockLive(m, "v6", func() (*models.Mock, error) { return view, nil })
		}, live: true},
		{name: "told of nothing", e: exchange(false, "5"), after: func(*Tracker) {}},
		{name: "served again, the second serve told of nothing", e: exchange(false, "5"), after: func(tr *Tracker) {
			tr.ServedMockAsRecorded(m)
			tr.ServedMock(m)
		}},
		{name: "another recording of its name and request time told", e: exchange(false, "5"), mocks: []*models.Mock{other, m},
			before: func(tr *Tracker) { tr.ServedMock(other); tr.ServedMockAsRecorded(other) }, after: func(*Tracker) {}},
		{name: "said as recorded, a leaf shows another value, told as recorded after", e: exchange(true, "6"), after: func(tr *Tracker) {
			tr.ServedMockAsRecorded(m)
		}},
	} {
		tr := NewTracker()
		tr.Begin()
		if c.before != nil {
			c.before(tr)
		}
		tr.ServedMock(m)
		tr.Served(tr.Epoch(), c.e)
		c.after(tr)
		mocks := c.mocks
		if mocks == nil {
			mocks = []*models.Mock{m}
		}
		p := tr.Placed()
		earlier := InServeOrder(p.Earlier, mocks, p.Order)
		if len(earlier) != 1 {
			t.Fatalf("%s: %d exchanges; want the one served", c.name, len(earlier))
		}
		out := earlier[0].lateOf(false)
		if len(out) != 2 {
			t.Fatalf("%s: %d Raws; want the payload and what F served at its answer's leaves", c.name, len(out))
		}
		for i, x := range out {
			if x.AsRecorded() != c.recorded || x.Live() != c.live || x.viewUnstated() != (!c.recorded && !c.live) {
				t.Errorf("%s, Raw %d: as recorded %v, a view %v, unstated %v; want as recorded %v, a view %v", c.name, i, x.AsRecorded(), x.Live(), x.viewUnstated(), c.recorded, c.live)
			}
		}
	}
}

// A recording F served is read with the views its adapters stated of each
// serve (Order.ServedView): the one view F gave where every serve the read
// saw was told with it (Tracker.ServedMockLive, Served of an exchange with a
// View), each view where they gave more than one, and as recorded where every
// serve was told so. A value that holds no word of a view then learns; one
// that holds a word only a view holds is refused, naming the recording as
// record's full round gave it (F gave that word there, the recording did
// not). A serve told of no view, a view that cannot be kept (no loader, no
// key, past the views a set keeps of one recording) or read, or a statement
// of the previous set, refuses the value's new word (fail closed: a false
// red, never a word F may have given passed as noise).
func TestOrderServedViewReadsTheViewItsAdaptersStatedOfEachServe(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const (
		learns     = ""
		unstated   = "where its adapter did not say what it gave the application there"
		unread     = "where keploy could not read what it gave the application"
		givenThere = "as record's full round gave it"
		inAView    = "serves that gave the application different views of it"
	)
	m := jobsMock(chanceSrcRec, "5")
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) { return jobsMock(id, "9"), nil }
	}
	benign, holding := view(chanceSrcRec), view("6c7d")
	broken := func() (*models.Mock, error) { return nil, errors.New("parked answer gone") }
	exchange := func(asRecorded bool, load func() (*models.Mock, error)) Earlier {
		return Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, AsRecorded: asRecorded, View: load, ViewKey: "v",
			Payload: noSet(clockedDrifts()).LazyRawOf(m, true, nil),
			Seen:    []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}}
	}
	for _, c := range []struct {
		name       string
		run        func(tr *Tracker)
		want       string // "" learns; else a part of the refusal
		asRecorded bool
	}{
		{"served once, told with a view that holds no word of the value", func(tr *Tracker) { tr.ServedMock(m); tr.ServedMockLive(m, "a", benign) }, learns, false},
		{"served once, told with a view that holds the value's word", func(tr *Tracker) { tr.ServedMock(m); tr.ServedMockLive(m, "a", holding) }, givenThere, false},
		{"served twice, told twice with the same view", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", holding)
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", benign) // the same key: the first loader stands for both
		}, givenThere, false},
		{"served twice, told with two views, the second holding the word", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", benign)
			tr.ServedMock(m)
			tr.ServedMockLive(m, "b", holding)
		}, inAView, false},
		{"served twice, once as recorded and once with a view that holds no word", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockAsRecorded(m)
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", benign)
		}, learns, false},
		{"served twice, once as recorded and once with a view that holds the word", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockAsRecorded(m)
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", holding)
		}, inAView, false},
		{"served twice, told once with a view", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", benign)
			tr.ServedMock(m)
		}, unstated, false},
		{"told with no loader", func(tr *Tracker) { tr.ServedMock(m); tr.ServedMockLive(m, "a", nil) }, unread, false},
		{"told with no key", func(tr *Tracker) { tr.ServedMock(m); tr.ServedMockLive(m, "", benign) }, unread, false},
		{"told with a view that cannot be read", func(tr *Tracker) { tr.ServedMock(m); tr.ServedMockLive(m, "a", broken) }, unread, false},
		{"told with more different views than a set keeps", func(tr *Tracker) {
			for i := 0; i <= maxLiveViews; i++ {
				tr.ServedMock(m)
				tr.ServedMockLive(m, strconv.Itoa(i), benign)
			}
		}, unread, false},
		{"told before its serve", func(tr *Tracker) { tr.ServedMockLive(m, "a", benign); tr.ServedMock(m) }, unstated, false},
		{"told in the previous set", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", benign)
			tr.NewSet()
			tr.Begin()
			tr.ServedMock(m)
		}, unstated, false},
		{"an exchange with a view", func(tr *Tracker) { tr.ServedMock(m); tr.Served(tr.Epoch(), exchange(false, holding)) }, givenThere, false},
		{"an exchange served as recorded, with a view too", func(tr *Tracker) { tr.ServedMock(m); tr.Served(tr.Epoch(), exchange(true, holding)) }, learns, true},
		{"an exchange with no view", func(tr *Tracker) { tr.ServedMock(m); tr.Served(tr.Epoch(), exchange(false, nil)) }, unstated, false},
	} {
		tr := NewTracker()
		tr.Begin()
		c.run(tr)
		p := tr.Placed()
		dc := noSet(clockedDrifts())
		first, last := p.Order.Of(m)
		r := p.Order.ServedView(m, dc.LazyRawOf(m, true, nil).ServedIn(first, last))
		if r.AsRecorded() != c.asRecorded {
			t.Errorf("%s: read as recorded %v; want %v", c.name, r.AsRecorded(), c.asRecorded)
		}
		got := ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, append(eStartup(), r), nil, nil, nil)
		switch {
		case c.want == learns && got[0].Kind == KindUnaccounted:
			t.Errorf("%s: refused (%q); want learned: no word of it stands in what F gave", c.name, got[0].Why)
		case c.want != learns && (got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, c.want) || !strings.Contains(got[0].Why, "mock-6")):
			t.Errorf("FALSE GREEN? %s: %s (%q); want refused naming mock-6: %q", c.name, got[0].Kind, got[0].Why, c.want)
		}
	}
}

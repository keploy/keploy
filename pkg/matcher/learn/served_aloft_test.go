package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording F served in a request of the application's that was still
// aloft when the call reached keploy (its answer had not started to go out:
// Flight.Land) gave the application nothing before the call, however early
// F claimed it: a worker's findAndModify the Mongo learn step claims, then
// learns and renders before it writes the answer and states its view. A
// call that reaches keploy in that span, on another connection, read the
// recording as served before it with no view stated, and refused a fresh
// word of its own there, as of a place keploy cannot tell what F gave at
// (FALSE RED: an insert's fresh id, refused at the worker's recording). So
// is one F served only after the call arrived (servedAfter), with no view
// stated yet. A serve whose request landed before the call arrived, one in
// a request a serve caused (its parser lands it), or one read with no
// arrival said, stays refused (fail closed). So under ClassifyGiven and
// ClassifyFollowing, as a place of the call's own test and another test's,
// and with F's view stated before the call's read too (the worker states it
// just before it writes the answer), the view holding the call's fresh word
// by chance, or a view keploy cannot read: read as of the arrival, F had
// given the application none of it, where a view it stated used to refuse
// the word (FALSE RED). The fail-closed cases are then refused naming the
// view.
func TestARecordingServedInARequestAloftWhenTheCallArrivedGaveItNothing(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const unstated = "where its adapter did not say what it gave the application there"
	const viewed = "as record's full round gave it, where the recording did not give the application that word"
	const unreadable = "where keploy could not read what it gave the application"
	m := jobsMock(chanceSrcRec, "5")
	view := func() (*models.Mock, error) { return jobsMock(chanceSrcLive, "5"), nil }
	tick := func() { time.Sleep(2 * time.Millisecond) }
	for _, c := range []struct {
		name    string
		learns  bool
		arrived bool
		serve   func(tr *Tracker, arrive, state func())
	}{
		{"claimed before the call arrived, its request landing after it", true, true, func(tr *Tracker, arrive, state func()) {
			w := tr.Lift()
			tr.ServedMockIn(m, w)
			arrive()
			state()
			w.Land()
		}},
		{"claimed before the call arrived, its request aloft at the read", true, true, func(tr *Tracker, arrive, state func()) {
			tr.ServedMockIn(m, tr.Lift())
			arrive()
			state()
		}},
		{"claimed after the call arrived, in no request", true, true, func(tr *Tracker, arrive, state func()) {
			arrive()
			tr.ServedMock(m)
			state()
		}},
		{"its request landed before the call arrived", false, true, func(tr *Tracker, arrive, state func()) {
			w := tr.Lift()
			tr.ServedMockIn(m, w)
			state()
			w.Land()
			arrive()
		}},
		{"claimed before the call arrived in a request a serve caused, not landed", false, true, func(tr *Tracker, arrive, state func()) {
			cause := tr.Lift()
			tr.ServedMockIn(jobsMock("0c1d2e3f", "1"), cause)
			state()
			tr.ServedMockIn(m, cause.LiftCaused())
			arrive()
			state()
		}},
		{"its request aloft, no arrival said", false, false, func(tr *Tracker, arrive, state func()) {
			tr.ServedMockIn(m, tr.Lift())
			arrive()
			state()
		}},
	} {
		for _, stated := range []struct {
			name, want string
			tell       func(tr *Tracker)
		}{
			{"no view", unstated, func(*Tracker) {}},
			{"a view", viewed, func(tr *Tracker) { tr.ServedMockLive(m, "v", view) }},
			{"a view keploy cannot read", unreadable, func(tr *Tracker) { tr.ServedMockLive(m, "", nil) }},
		} {
			tr := NewTracker()
			tr.Begin()
			var arrival time.Time
			c.serve(tr, func() { tick(); arrival = time.Now(); tick() }, func() { stated.tell(tr) })
			o := tr.Placed().Order
			if c.arrived {
				o = o.ArrivedAt(arrival)
			}
			first, last := o.Of(m)
			for _, other := range []bool{false, true} {
				x := o.ServedView(m, RawOf(m, true).ServedIn(first, last))
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
					case c.learns && got.Kind == KindUnaccounted:
						t.Errorf("FALSE RED: %s, %s, %s stated, another test's %v: refused (%q); want learned", cl.name, c.name, stated.name, other, got.Why)
					case !c.learns && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, stated.want)):
						t.Errorf("FALSE GREEN: %s, %s, %s stated, another test's %v: %s (%q); want refused: %q", cl.name, c.name, stated.name, other, got.Kind, got.Why, stated.want)
					}
				}
			}
		}
	}
}

// Two serves F made of one recording, each told with its own view (keys v
// and w), are read as of the call's arrival too. Where both came after it
// (claimed after it in no request, or before it in requests still aloft
// then), F had given the application neither view when the call reached
// keploy, so the call's fresh word that stands in one of them by chance is
// learned, under ClassifyGiven and ClassifyFollowing, as a place of the
// call's own test and another test's: read with the views it was refused as
// in one of two serves that gave the application different views of it
// (FALSE RED). Where both requests landed before the arrival, or one did,
// it stays refused naming the two views (fail closed).
func TestTwoViewsOfARecordingServedAloftWhenTheCallArrivedGaveItNothing(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const inTwo = "in one of 2 serves that gave the application different views of it"
	m := jobsMock(chanceSrcRec, "5")
	v := func() (*models.Mock, error) { return jobsMock(chanceSrcLive, "5"), nil }
	w := func() (*models.Mock, error) { return jobsMock("1a2b3c4d-5e6f-4a7b-8c9d-e0f1a2b3c4d5", "5"), nil }
	tick := func() { time.Sleep(2 * time.Millisecond) }
	for _, c := range []struct {
		name   string
		learns bool
		serve  func(tr *Tracker, arrive func())
	}{
		{"claimed twice after the call arrived, in no request", true, func(tr *Tracker, arrive func()) {
			arrive()
			tr.ServedMock(m)
			tr.ServedMockLive(m, "v", v)
			tr.ServedMock(m)
			tr.ServedMockLive(m, "w", w)
		}},
		{"claimed twice before the call arrived, both requests landing after it", true, func(tr *Tracker, arrive func()) {
			a, b := tr.Lift(), tr.Lift()
			tr.ServedMockIn(m, a)
			tr.ServedMockIn(m, b)
			arrive()
			tr.ServedMockLive(m, "v", v)
			a.Land()
			tr.ServedMockLive(m, "w", w)
			b.Land()
		}},
		{"served twice, both requests landed before the call arrived", false, func(tr *Tracker, arrive func()) {
			a := tr.Lift()
			tr.ServedMockIn(m, a)
			tr.ServedMockLive(m, "v", v)
			a.Land()
			b := tr.Lift()
			tr.ServedMockIn(m, b)
			tr.ServedMockLive(m, "w", w)
			b.Land()
			arrive()
		}},
		{"served twice, one request landed before the call arrived, one after it", false, func(tr *Tracker, arrive func()) {
			a := tr.Lift()
			tr.ServedMockIn(m, a)
			tr.ServedMockLive(m, "v", v)
			a.Land()
			b := tr.Lift()
			tr.ServedMockIn(m, b)
			arrive()
			tr.ServedMockLive(m, "w", w)
			b.Land()
		}},
	} {
		tr := NewTracker()
		tr.Begin()
		var arrival time.Time
		c.serve(tr, func() { tick(); arrival = time.Now(); tick() })
		o := tr.Placed().Order.ArrivedAt(arrival)
		first, last := o.Of(m)
		for _, other := range []bool{false, true} {
			x := o.ServedView(m, RawOf(m, true).ServedIn(first, last))
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
				case c.learns && got.Kind == KindUnaccounted:
					t.Errorf("FALSE RED: %s, %s, another test's %v: refused (%q); want learned", cl.name, c.name, other, got.Why)
				case !c.learns && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, inTwo)):
					t.Errorf("FALSE GREEN: %s, %s, another test's %v: %s (%q); want refused: %q", cl.name, c.name, other, got.Kind, got.Why, inTwo)
				}
			}
		}
	}
}

// A serve told as recorded keeps that statement as of the call's arrival,
// even where every serve the read saw came after it (servedAfter): what it
// gave the application is the recording itself, which holds no word the
// recorded text does not. Flyway's execution_time, recorded as 50 beside the
// history table's VARCHAR(50) and bound 7 in F, is then still no copy of it
// (a copy moves with its source) and is learned as varying, naming the
// reading set aside, with the serve claimed after the call arrived. Read
// with no view, the place would stay the value's source, and the call would
// be refused for a value it never copied (FALSE RED).
func TestASourceToldAsRecordedAndServedAfterTheCallArrivedStaysAsRecorded(t *testing.T) {
	before := eRecAt.Add(-150 * time.Millisecond)
	m := &models.Mock{Name: "mock-5"}
	tr := NewTracker()
	tr.Begin()
	time.Sleep(2 * time.Millisecond)
	arrival := time.Now()
	time.Sleep(2 * time.Millisecond)
	tr.ServedMock(m)
	tr.ServedMockAsRecorded(m)
	o := tr.Placed().Order.ArrivedAt(arrival)
	first, last := o.Of(m)
	x := o.ServedView(m, eHistory(before, 0, false).ServedIn(first, last))
	if !x.AsRecorded() {
		t.Fatalf("the serve told as recorded, claimed after the call arrived, is read as %+v; want as recorded", x)
	}
	got := classifyF(t, "50", "7", nil, append(eStartup(), x), nil)
	if got.Kind != KindNoise || !strings.Contains(got.Why, "which record's full round sent and gave the application as recorded, while the value changed") {
		t.Errorf("FALSE RED: 50 beside VARCHAR(50) F served as recorded after the call arrived, 7 in F: %s (%q); want noise naming mock-5", got.Kind, got.Why)
	}
}

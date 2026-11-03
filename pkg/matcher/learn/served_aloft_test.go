package learn

import (
	"strings"
	"testing"
	"time"
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
// ClassifyFollowing, as a place of the call's own test and another test's.
func TestARecordingServedInARequestAloftWhenTheCallArrivedGaveItNothing(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const unstated = "where its adapter did not say what it gave the application there"
	m := jobsMock(chanceSrcRec, "5")
	tick := func() { time.Sleep(2 * time.Millisecond) }
	for _, c := range []struct {
		name    string
		learns  bool
		arrived bool
		serve   func(tr *Tracker, arrive func())
	}{
		{"claimed before the call arrived, its request landing after it", true, true, func(tr *Tracker, arrive func()) {
			w := tr.Lift()
			tr.ServedMockIn(m, w)
			arrive()
			w.Land()
		}},
		{"claimed before the call arrived, its request aloft at the read", true, true, func(tr *Tracker, arrive func()) {
			tr.ServedMockIn(m, tr.Lift())
			arrive()
		}},
		{"claimed after the call arrived, in no request", true, true, func(tr *Tracker, arrive func()) {
			arrive()
			tr.ServedMock(m)
		}},
		{"its request landed before the call arrived", false, true, func(tr *Tracker, arrive func()) {
			w := tr.Lift()
			tr.ServedMockIn(m, w)
			w.Land()
			arrive()
		}},
		{"claimed before the call arrived in a request a serve caused, not landed", false, true, func(tr *Tracker, arrive func()) {
			cause := tr.Lift()
			tr.ServedMockIn(jobsMock("0c1d2e3f", "1"), cause)
			tr.ServedMockIn(m, cause.LiftCaused())
			arrive()
		}},
		{"its request aloft, no arrival said", false, false, func(tr *Tracker, arrive func()) {
			tr.ServedMockIn(m, tr.Lift())
			arrive()
		}},
	} {
		tr := NewTracker()
		tr.Begin()
		var arrival time.Time
		c.serve(tr, func() { tick(); arrival = time.Now(); tick() })
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
					t.Errorf("FALSE RED: %s, %s, another test's %v: refused (%q); want learned", cl.name, c.name, other, got.Why)
				case !c.learns && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, unstated)):
					t.Errorf("FALSE GREEN: %s, %s, another test's %v: %s (%q); want refused: %q", cl.name, c.name, other, got.Kind, got.Why, unstated)
				}
			}
		}
	}
}

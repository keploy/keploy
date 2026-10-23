package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func beforeNames(t *Tracker) []string {
	_, _, _, before := t.Test()
	var out []string
	for _, m := range before {
		out = append(out, m.Name)
	}
	return out
}

// A mock served while no test is open (the start-up, between tests) is an
// earlier place of every later test, once; one served in a test is one of
// the next test only, as before.
func TestTrackerKeepsWhatWasServedOutsideATestForEveryLaterTest(t *testing.T) {
	tr := NewTracker()
	tr.ServedMock(&models.Mock{Name: "startup"})
	tr.Begin() // test 1
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"startup"}) {
		t.Fatalf("test 1: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-1"})
	tr.End()
	tr.ServedMock(&models.Mock{Name: "between"})
	tr.ServedMock(&models.Mock{Name: "startup"}) // served again
	tr.Begin()                                   // test 2
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-1", "between", "startup"}) {
		t.Fatalf("test 2: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-2"})
	tr.End()
	tr.Begin() // test 3
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-2", "startup", "between"}) {
		t.Fatalf("test 3: before %v, want test 2's own and everything served outside a test", got)
	}
}

// A new test set forgets what the previous set served: its mocks reuse the
// previous set's names (mock-0 again), and none of the previous set's is an
// earlier place of the new set's tests. Without NewSet, a mock is still kept
// by its identity, not its name: another set's mock-0 never hides this one's.
func TestTrackerTellsTestSetsApart(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 10, 16, s, 0, 0, 0, time.UTC) }
	set1 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(1)}}
	set2 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(2)}}
	for _, newSet := range []bool{true, false} {
		tr := NewTracker()
		tr.ServedMock(set1)
		tr.Begin()
		tr.End()
		if newSet {
			tr.NewSet()
		}
		tr.ServedMock(set2)
		tr.Begin()
		tr.End()
		tr.Begin() // set 2, test 2
		_, _, _, before := tr.Test()
		var has1, has2 bool
		for _, m := range before {
			has1, has2 = has1 || m == set1, has2 || m == set2
		}
		if !has2 || has1 == newSet {
			t.Errorf("NewSet %v: set 2's test 2 has set 1's mock-0 %v, set 2's %v", newSet, has1, has2)
		}
	}
}

// The start-up's calls share one drift cache, so each earlier recording's
// text is read once there; a test, and a new set, start afresh.
func TestTrackerKeepsADriftCacheForTheStartUp(t *testing.T) {
	tr := NewTracker()
	startUp := tr.Drifts()
	if startUp == nil || tr.Drifts() != startUp {
		t.Fatal("the start-up's calls share no drift cache")
	}
	tr.Begin()
	test := tr.Drifts()
	if test == startUp {
		t.Fatal("the first test read the start-up's cache")
	}
	tr.End()
	tr.NewSet()
	if d := tr.Drifts(); d == nil || d == startUp || d == test {
		t.Fatal("a new set's start-up has no cache of its own")
	}
}

// A call's window runs, in the recording, from the set's previous
// data-plane recording to the paired one, and in this run, on the app's
// clock, from the previous data-plane call the store served to the call's
// arrival. A protocol-phase call is no previous call, in either run.
func TestTrackerCallWindow(t *testing.T) {
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	at := func(name string, d time.Duration, kind models.Kind, config bool) *models.Mock {
		m := &models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{ReqTimestampMock: rec.Add(d), Metadata: map[string]string{}}}
		if config {
			m.Spec.Metadata["type"] = "config"
		}
		return m
	}
	boot := at("mock-0", -time.Second, models.Mongo, false)
	receive := at("mock-1", 0, models.HTTP, false)
	hello := at("mock-2", 2*time.Millisecond, models.Mongo, true)
	insert := at("mock-4", 4*time.Millisecond, models.Mongo, false)
	later := at("mock-5", 9*time.Millisecond, models.HTTP, false)
	recorded := []*models.Mock{insert, later, receive, hello, boot}

	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	offset := 21 * time.Second
	clock := func(r time.Time) (time.Time, bool) { return r.Add(offset), true }

	tr := NewTracker()
	if w := tr.CallWindow(insert, recorded, base.Add(3*time.Second)); w != nil {
		t.Fatalf("a window with no app clock: %+v", w)
	}
	tr.SetAppClock(clock)
	if w := tr.CallWindow(insert, recorded, base.Add(3*time.Second)); w != nil {
		t.Fatalf("a window with no call served before: %+v", w)
	}
	tr.ServedMockAt(receive, base.Add(2*time.Second))
	tr.ServedMockAt(hello, base.Add(2*time.Second+time.Millisecond))
	// Served after the insert arrived, on another connection.
	tr.ServedMockAt(later, base.Add(4*time.Second))
	w := tr.CallWindow(insert, recorded, base.Add(2*time.Second+3*time.Millisecond))
	want := &Window{
		Rec: Span{From: receive.Spec.ReqTimestampMock, To: insert.Spec.ReqTimestampMock},
		F:   Span{From: base.Add(2*time.Second + offset), To: base.Add(2*time.Second + 3*time.Millisecond + offset)},
	}
	if !reflect.DeepEqual(w, want) {
		t.Fatalf("window %+v; want %+v", w, want)
	}
	// The set's first data-plane call has no previous call recorded.
	if w := tr.CallWindow(boot, recorded, base.Add(time.Second)); w != nil {
		t.Fatalf("the first call's window: %+v", w)
	}
	// A clock that cannot read an instant gives no window.
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, r.After(base.Add(2*time.Second + time.Millisecond)) })
	if w := tr.CallWindow(insert, recorded, base.Add(2*time.Second+3*time.Millisecond)); w != nil {
		t.Fatalf("a window over an unread instant: %+v", w)
	}
	// A new set forgets what the last one served.
	tr.SetAppClock(clock)
	tr.NewSet()
	if w := tr.CallWindow(insert, recorded, base.Add(5*time.Second)); w != nil {
		t.Fatalf("a window across sets: %+v", w)
	}
	// A recording with no recorded time has no window.
	tr.ServedMockAt(receive, base)
	if w := tr.CallWindow(&models.Mock{Name: "mock-9", Kind: models.Mongo}, recorded, base.Add(time.Second)); w != nil {
		t.Fatalf("a window of no recorded time: %+v", w)
	}
}

// Serves the store reports out of order (two connections) are read in the
// order they happened: the previous call is the latest served before the
// arrival, whichever was reported first.
func TestTrackerCallWindowReadsServesInTheirOrder(t *testing.T) {
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	data := func(name string, d time.Duration) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec.Add(d)}}
	}
	prev, paired := data("mock-0", 0), data("mock-1", 4*time.Millisecond)
	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.ServedMockAt(data("late", time.Second), base.Add(5*time.Second))
	tr.ServedMockAt(prev, base.Add(2*time.Second))
	tr.ServedMockAt(data("early", -time.Second), base)
	w := tr.CallWindow(paired, []*models.Mock{prev}, base.Add(3*time.Second))
	if w == nil || !w.F.From.Equal(base.Add(2*time.Second)) {
		t.Fatalf("window %+v; want F from the serve at +2s, the latest before the arrival", w)
	}
}

// A window is one run of the app's clock: if the clock was re-written
// between the previous serve and the arrival (a test's begin), there is
// none, though each instant reads.
func TestTrackerCallWindowIsOneRunOfTheAppsClock(t *testing.T) {
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	prev := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec}}
	paired := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec.Add(4 * time.Millisecond)}}
	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	for _, jump := range []time.Duration{0, 300 * time.Second, -300 * time.Second} {
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) {
			if r.Before(base.Add(time.Millisecond)) {
				return r.Add(time.Minute), true
			}
			return r.Add(time.Minute + jump), true
		})
		tr.ServedMockAt(prev, base)
		w := tr.CallWindowFrom(prev.Spec.ReqTimestampMock, paired, base.Add(3*time.Millisecond))
		if (w != nil) != (jump == 0) {
			t.Fatalf("jump %v: window %+v; want one only with one offset", jump, w)
		}
	}
	// A previous recorded call no earlier than the paired one gives none.
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.ServedMockAt(prev, base)
	if w := tr.CallWindowFrom(paired.Spec.ReqTimestampMock, paired, base.Add(time.Millisecond)); w != nil {
		t.Fatalf("a previous call at the paired one's own time gave %+v", w)
	}
}

// The running test's number (Epoch) moves on Begin and NewSet and on nothing
// else: an End, a serve and an answer keep it. It is Test's number, and a nil
// tracker's is 0. A learn step reads it as a call is matched and as it
// decides, and equal numbers are one test of one set.
func TestTrackerEpochMovesOnlyWithTheRunningTest(t *testing.T) {
	var none *Tracker
	if got := none.Epoch(); got != 0 {
		t.Fatalf("a nil tracker's epoch is %d; want 0", got)
	}
	tr := NewTracker()
	same := func(step string, want uint64) {
		t.Helper()
		if got := tr.Epoch(); got != want {
			t.Fatalf("after %s the epoch is %d; want %d", step, got, want)
		}
		if n, _, _, _ := tr.Test(); n != want {
			t.Fatalf("after %s Test's number is %d; want the epoch, %d", step, n, want)
		}
	}
	e := tr.Epoch()
	same("NewTracker", e)
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP}
	tr.ServedMock(m)
	same("a start-up serve", e)
	tr.Begin()
	e1 := tr.Epoch()
	if e1 == e {
		t.Fatalf("Begin kept the epoch %d", e)
	}
	same("Begin", e1)
	tr.ServedMock(m)
	tr.Answer(e1, m)
	same("a serve and an answer", e1)
	tr.End()
	same("End", e1)
	tr.Begin()
	e2 := tr.Epoch()
	if e2 == e1 {
		t.Fatalf("the next Begin kept the epoch %d", e1)
	}
	tr.NewSet()
	if e3 := tr.Epoch(); e3 == e2 {
		t.Fatalf("NewSet kept the epoch %d", e2)
	}
}

// The open test's number (OpenEpoch) is the epoch while a test is open, from
// Begin until End or NewSet, and 0 while none is: before the first Begin,
// after End, after NewSet. It is never 0 for an open test (Begin moves the
// epoch off 0), so a store that stamps a test's view with it can say "no
// test of the tracker's" with 0.
func TestTrackerOpenEpochIsTheEpochOnlyWhileATestIsOpen(t *testing.T) {
	var none *Tracker
	if got := none.OpenEpoch(); got != 0 {
		t.Fatalf("a nil tracker's open epoch is %d; want 0", got)
	}
	tr := NewTracker()
	closed := func(step string) {
		t.Helper()
		if got := tr.OpenEpoch(); got != 0 {
			t.Fatalf("after %s, with no test open, the open epoch is %d; want 0", step, got)
		}
	}
	open := func(step string) {
		t.Helper()
		e := tr.Epoch()
		if e == 0 {
			t.Fatalf("after %s an open test's epoch is 0", step)
		}
		if got := tr.OpenEpoch(); got != e {
			t.Fatalf("after %s the open epoch is %d; want the epoch, %d", step, got, e)
		}
	}
	closed("NewTracker")
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP}
	tr.ServedMock(m)
	closed("a start-up serve")
	tr.Begin()
	open("Begin")
	tr.ServedMock(m)
	tr.Answer(tr.Epoch(), m)
	open("a serve and an answer")
	tr.End()
	closed("End")
	tr.ServedMock(m)
	closed("a serve between tests")
	tr.Begin()
	open("the next Begin")
	tr.NewSet()
	closed("NewSet")
	tr.Begin()
	open("the new set's first Begin")
}

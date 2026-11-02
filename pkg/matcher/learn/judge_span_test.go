package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// spanRun is a tracker on an app clock that reads real time, whose store
// says the recordings named in own are the open test's own or the
// start-up's (SetServeOwner); every other recording is a worker:,
// background or pool one.
func spanRun(t *testing.T, own ...string) *Tracker {
	t.Helper()
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	mine := map[string]bool{}
	for _, n := range own {
		mine[n] = true
	}
	tr.SetServeOwner(func(m *models.Mock) bool { return mine[m.Name] })
	return tr
}

func rec(name string) *models.Mock { return &models.Mock{Name: name, Kind: models.Mongo} }

func spanFrom(t *testing.T, what string, s Span, ok bool, want time.Time) {
	t.Helper()
	if !ok || !s.From.Equal(want) {
		t.Fatalf("%s: span %+v, %v; want it from %v", what, s, ok, want)
	}
}

// A worker: serve that lands between a test call's clock read and its send
// does not start the span of the call's check against the test's own
// recording: the read, made after the test's own previous serve, passes. A
// check against the worker: recording, and LiveSpan, keep the span from any
// flow's serve. A start-up serve made after the test began starts it, as the
// test's own does.
func TestJudgeSpan_AWorkerServeDoesNotStartTheSpanOfACheckAgainstTheTestsOwnRecording(t *testing.T) {
	began := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ownServe, workerServe, arrival := began.Add(10*time.Millisecond), began.Add(20*time.Millisecond), began.Add(30*time.Millisecond)
	tr := spanRun(t, "own-1", "own-2", "start-1")
	tr.NewSetAt(began.Add(-time.Second))
	tr.BeginAt(began)
	tr.ServedMockAt(rec("own-1"), ownServe)
	tr.ServedMockAt(rec("worker-1"), workerServe)
	s, ok := tr.LiveSpanOf(arrival, rec("own-2"))
	spanFrom(t, "checked against the test's own recording", s, ok, ownServe)
	if read := workerServe.Add(-5 * time.Millisecond); read.Before(s.From) || read.After(s.To) {
		t.Fatalf("a read at %v, between the test's own serve and the worker: serve, is outside the span %+v", read, s)
	}
	s, ok = tr.LiveSpanOf(arrival, rec("worker-2"))
	spanFrom(t, "checked against a worker: recording", s, ok, workerServe)
	s, ok = tr.LiveSpan(arrival)
	spanFrom(t, "LiveSpan", s, ok, workerServe)

	startServe := began.Add(25 * time.Millisecond)
	tr.ServedMockAt(rec("start-1"), startServe)
	s, ok = tr.LiveSpanOf(arrival, rec("own-2"))
	spanFrom(t, "after a start-up serve", s, ok, startServe)
}

// The span of a test's own call never starts before the latest serve of any
// flow made before the test began: a read the application kept from TestA
// (its last serve a worker: one) and sent in TestB's first call is before
// the span, and refused, as LiveSpan has it. A worker: serve after TestB
// began moves LiveSpan's start, not this one's.
func TestJudgeSpan_AReadCarriedFromTheTestBeforeIsRefused(t *testing.T) {
	aBegan := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	tr := spanRun(t, "a-own", "b-own")
	tr.NewSetAt(aBegan.Add(-time.Second))
	tr.BeginAt(aBegan)
	tr.ServedMockAt(rec("a-own"), aBegan.Add(10*time.Millisecond))
	aLast := aBegan.Add(20 * time.Millisecond)
	tr.ServedMockAt(rec("worker-1"), aLast)
	tr.End()
	bBegan := aBegan.Add(time.Second)
	tr.BeginAt(bBegan)
	arrival := bBegan.Add(5 * time.Millisecond)
	s, ok := tr.LiveSpanOf(arrival, rec("b-own"))
	spanFrom(t, "TestB's first call", s, ok, aLast)
	main, mok := tr.LiveSpan(arrival)
	spanFrom(t, "LiveSpan of TestB's first call", main, mok, aLast)
	if read := aBegan.Add(15 * time.Millisecond); !read.Before(s.From) {
		t.Fatalf("a read at %v, made in TestA before its last serve, is inside TestB's span %+v", read, s)
	}
	tr.ServedMockAt(rec("worker-2"), bBegan.Add(2*time.Millisecond))
	s, ok = tr.LiveSpanOf(arrival, rec("b-own"))
	spanFrom(t, "after a worker: serve in TestB", s, ok, aLast)
	s, ok = tr.LiveSpan(arrival)
	spanFrom(t, "LiveSpan after a worker: serve in TestB", s, ok, bBegan.Add(2*time.Millisecond))
}

// Record's learn step keeps its window from any flow's serve: a worker:
// serve between the test's own serve and the call starts the call's F span
// (CallWindowFrom), whatever the store says of the recording.
func TestJudgeSpan_LearnKeepsTheSpanFromAnyFlowsServe(t *testing.T) {
	began := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	tr := spanRun(t, "own-1", "own-2")
	tr.NewSetAt(began.Add(-time.Second))
	tr.BeginAt(began)
	tr.ServedMockAt(rec("own-1"), began.Add(10*time.Millisecond))
	workerServe := began.Add(20 * time.Millisecond)
	tr.ServedMockAt(rec("worker-1"), workerServe)
	paired := rec("own-2")
	recAt := began.Add(-time.Hour)
	paired.Spec.ReqTimestampMock = recAt
	w := tr.CallWindowFrom(recAt.Add(-time.Second), paired, began.Add(30*time.Millisecond))
	if w == nil || !w.F.From.Equal(workerServe) {
		t.Fatalf("learn's window %+v; want F from the worker: serve %v", w, workerServe)
	}
}

// With no owner told (no store's view: the switch off, keploy test without
// exact serving), or no test open, LiveSpanOf is LiveSpan.
func TestJudgeSpan_WithNoOwnerOrNoTestOpenItIsLiveSpan(t *testing.T) {
	began := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	arrival := began.Add(30 * time.Millisecond)
	for _, tc := range []struct {
		name  string
		owner bool
		open  bool
	}{{"no owner told", false, true}, {"no test open", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			tr := spanRun(t, "own-1", "own-2")
			if !tc.owner {
				tr.SetServeOwner(nil)
			}
			tr.NewSetAt(began.Add(-time.Second))
			tr.BeginAt(began)
			tr.ServedMockAt(rec("own-1"), began.Add(10*time.Millisecond))
			tr.ServedMockAt(rec("worker-1"), began.Add(20*time.Millisecond))
			if !tc.open {
				tr.End()
			}
			s, ok := tr.LiveSpanOf(arrival, rec("own-2"))
			spanFrom(t, "LiveSpanOf", s, ok, began.Add(20*time.Millisecond))
		})
	}
}

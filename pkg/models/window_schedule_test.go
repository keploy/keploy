package models

import (
	"testing"
	"time"
)

var sched0 = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return sched0.Add(time.Duration(ms) * time.Millisecond) }

// A long early window (test-2, 200..5000) that later tests overlap, as in the
// production recording where 814 MESSAGEs sit in one such window. Deliberately
// given out of Start order: the constructor sorts.
func overlapSchedule() *WindowSchedule {
	return NewWindowSchedule([]TestWindow{
		{TestCase: "test-3", Start: at(1000), End: at(1100)},
		{TestCase: "test-1", Start: at(100), End: at(150)},
		{TestCase: "test-2", Start: at(200), End: at(5000)},
		{TestCase: "test-4", Start: at(6000), End: at(6100)},
		{TestCase: "bad-zero", End: at(7000)},                   // dropped: no start
		{TestCase: "test-5", Start: at(8000), End: at(7000)},    // inverted: clamped
		{TestCase: "test-6", Start: at(9000), End: time.Time{}}, // zero end: clamped
	})
}

func TestWindowScheduleSortsAndCleans(t *testing.T) {
	s := overlapSchedule()
	want := []string{"test-1", "test-2", "test-3", "test-4", "test-5", "test-6"}
	if s.Len() != len(want) {
		t.Fatalf("Len = %d, want %d", s.Len(), len(want))
	}
	for i, name := range want {
		if got := s.Window(i).TestCase; got != name {
			t.Fatalf("window %d = %s, want %s", i, got, name)
		}
	}
	if !s.Window(4).End.Equal(at(8000)) || !s.Window(5).End.Equal(at(9000)) {
		t.Fatalf("inverted/zero ends not clamped to start: %v %v", s.Window(4).End, s.Window(5).End)
	}
}

func TestWindowScheduleReleaseIndex(t *testing.T) {
	s := overlapSchedule()
	for _, tc := range []struct {
		name string
		t    time.Time
		want int
	}{
		{"before every window is startup", at(50), -1},
		{"inside test-1", at(120), 0},
		{"gap after test-1 belongs to test-1", at(170), 0},
		{"inside the long window only", at(300), 1},
		// test-3 [1000,1100] also contains 1050, but the long window is the
		// FIRST containing one. Releasing at test-3 would make test-2's own
		// handler wait on it.
		{"inside the long window AND test-3 releases at the long window", at(1050), 1},
		{"after test-3 but still inside the long window", at(4000), 1},
		{"gap after the long window belongs to the latest started (test-3)", at(5500), 2},
		{"exact start of test-4", at(6000), 3},
		{"exact end of test-4", at(6100), 3},
		{"after the last window", at(20000), 5},
	} {
		if got := s.ReleaseIndex(tc.t); got != tc.want {
			t.Errorf("%s: ReleaseIndex = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestWindowScheduleReleasedAndHorizon(t *testing.T) {
	s := overlapSchedule()
	// While test-3 is current (index 2): everything up to the long window's
	// end is released (maxEnd 5000 beats test-4's start - 1ns), and the gap
	// after it too, until test-4 starts.
	cur := at(1000)
	h, ok := s.ReleaseHorizon(cur)
	if !ok || !h.Equal(at(6000).Add(-time.Nanosecond)) {
		t.Fatalf("horizon at test-3 = %v,%v; want test-4 start - 1ns", h, ok)
	}
	for _, tc := range []struct {
		t    time.Time
		want bool
	}{
		{at(50), true}, {at(4999), true}, {at(5999), true}, {at(6000), false}, {at(9500), false},
	} {
		if got := s.Released(tc.t, cur); got != tc.want {
			t.Errorf("Released(%v) at test-3 = %v, want %v", tc.t.Sub(sched0), got, tc.want)
		}
	}
	// While test-1 is current: the long window has not started, so its
	// traffic waits. Horizon = test-2 start - 1ns.
	h, ok = s.ReleaseHorizon(at(100))
	if !ok || !h.Equal(at(200).Add(-time.Nanosecond)) {
		t.Fatalf("horizon at test-1 = %v,%v; want test-2 start - 1ns", h, ok)
	}
	if s.Released(at(300), at(100)) {
		t.Fatal("the long window's traffic was released while test-1 ran")
	}
	// A current start before every window (staging) releases only startup.
	h, ok = s.ReleaseHorizon(at(10))
	if !ok || !h.Equal(at(100).Add(-time.Nanosecond)) {
		t.Fatalf("horizon before test-1 = %v,%v", h, ok)
	}
	// The last window's horizon is unbounded.
	if _, ok := s.ReleaseHorizon(at(9000)); ok {
		t.Fatal("the last window must have an unbounded horizon")
	}
	// A current start that falls between recorded windows (an unselected test
	// was skipped) counts as the latest window started by then.
	if !s.Released(at(5500), at(5800)) {
		t.Fatal("the gap after test-3 must be released once the replay is past test-3")
	}
}

// The horizon must agree with Released at every boundary; a brute-force walk
// over a fine grid catches an off-by-one in either.
func TestWindowScheduleHorizonMatchesReleased(t *testing.T) {
	s := overlapSchedule()
	for curMs := 0; curMs <= 10000; curMs += 50 {
		cur := at(curMs)
		h, bounded := s.ReleaseHorizon(cur)
		for tMs := 0; tMs <= 10000; tMs += 25 {
			tt := at(tMs)
			want := s.Released(tt, cur)
			got := !bounded || !tt.After(h)
			if got != want {
				t.Fatalf("cur=%dms t=%dms: horizon says %v, Released says %v (horizon %v)", curMs, tMs, got, want, h.Sub(sched0))
			}
		}
	}
}

func TestWindowScheduleNilAndEmpty(t *testing.T) {
	var s *WindowSchedule
	if s.Len() != 0 || s.CurrentIndex(at(1)) != -1 || s.ReleaseIndex(at(1)) != -1 {
		t.Fatal("nil schedule must be empty")
	}
	if !s.Released(at(100), at(0)) {
		t.Fatal("with no schedule everything is released")
	}
	if _, ok := s.ReleaseHorizon(at(0)); ok {
		t.Fatal("with no schedule the horizon is unbounded")
	}
	e := NewWindowSchedule(nil)
	if e.Len() != 0 || !e.Released(at(100), at(0)) {
		t.Fatal("empty schedule must behave as nil")
	}
}

// The gap before a window runs from the latest end of the windows that start
// before it to its start, exclusive at both ends; the first window has none
// (what precedes it is start-up traffic), and neither has a window an earlier
// one reaches, nor a start no window has. A call sent in a gap and answered at
// or after the next window's start was in flight when that test began; one
// sent and answered in the gap, one sent inside an earlier window, and one
// sent before the gap's start are not.
func TestWindowScheduleGapBeforeAndInFlight(t *testing.T) {
	s := NewWindowSchedule([]TestWindow{
		{TestCase: "test-1", Start: at(1000), End: at(1100)},
		{TestCase: "test-2", Start: at(2000), End: at(2100)},
		{TestCase: "long", Start: at(3000), End: at(5000)},
		{TestCase: "inside", Start: at(4000), End: at(4100)},
	})
	if from, ok := s.GapBefore(at(2000)); !ok || !from.Equal(at(1100)) {
		t.Fatalf("gap before test-2 from %v ok=%v; want from test-1's end", from, ok)
	}
	for _, c := range []struct {
		name  string
		start time.Time
	}{{"the first window", at(1000)}, {"a window an earlier one reaches", at(4000)}, {"a start no window has", at(2500)}} {
		if _, ok := s.GapBefore(c.start); ok {
			t.Fatalf("%s has a gap", c.name)
		}
	}
	together := NewWindowSchedule([]TestWindow{
		{TestCase: "test-1", Start: at(1000), End: at(1100)},
		{TestCase: "b", Start: at(2000), End: at(2100)},
		{TestCase: "c", Start: at(2000), End: at(2200)},
	})
	if _, ok := together.GapBefore(at(2000)); ok || together.InFlightInto(at(1500), at(2050), at(2000)) {
		t.Fatal("two windows that start together have a gap: which test a call in flight then is cannot be told")
	}
	var nilSched *WindowSchedule
	if _, ok := nilSched.GapBefore(at(2000)); ok || nilSched.InFlightInto(at(1500), at(2050), at(2000)) {
		t.Fatal("no schedule has a gap")
	}
	for _, c := range []struct {
		name     string
		req, res int
		want     bool
	}{
		{"sent in the gap, answered in the window", 1500, 2050, true},
		{"sent in the gap, answered at its start", 1500, 2000, true},
		{"sent in the gap, answered after the window", 1500, 2500, true},
		{"sent and answered in the gap", 1500, 1900, false},
		{"sent at the gap's start (test-1's end)", 1100, 2050, false},
		{"sent inside test-1", 1050, 2050, false},
		{"sent at the window's start", 2000, 2050, false},
	} {
		if got := s.InFlightInto(at(c.req), at(c.res), at(2000)); got != c.want {
			t.Errorf("%s: InFlightInto = %v, want %v", c.name, got, c.want)
		}
	}
	if s.InFlightInto(at(1500), time.Time{}, at(2000)) {
		t.Error("a call with no answer time was in flight")
	}
}

// After is past every window's End, the latest End and not the last-started
// window's: a long early window that a later, shorter one starts inside still
// holds what was recorded before its own End. A moment at that End is the
// window's (Contains), not after it. An empty schedule and a zero moment tell
// no teardown.
func TestWindowScheduleAfterIsPastEveryWindowsEnd(t *testing.T) {
	s := NewWindowSchedule([]TestWindow{
		{TestCase: "test-2", Start: at(200), End: at(300)},
		{TestCase: "test-1", Start: at(100), End: at(5000)},
	})
	for _, c := range []struct {
		ms   int
		want bool
	}{{50, false}, {150, false}, {250, false}, {400, false}, {5000, false}, {5001, true}, {9000, true}} {
		if got := s.After(at(c.ms)); got != c.want {
			t.Fatalf("After(%d) = %v, want %v", c.ms, got, c.want)
		}
	}
	if s.After(time.Time{}) {
		t.Fatal("a zero moment is after the windows")
	}
	var none *WindowSchedule
	if none.After(at(9000)) || NewWindowSchedule(nil).After(at(9000)) {
		t.Fatal("with no schedule a moment is after the windows")
	}
}

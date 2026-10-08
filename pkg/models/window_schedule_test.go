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

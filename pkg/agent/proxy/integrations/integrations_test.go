package integrations

import (
	"testing"
	"time"
)

// A call's mark is moved, at a later read of the round, by another staging
// or another boundary count of the round, and by nothing else: not by more
// takes, nor by a generation that moved alone (a test process's claim of its
// key again). A mark whose store keeps no generation is moved at every read
// (nothing tells the round did not move under it: fail closed); the zero
// CallRound, of no read, is never moved, its later read being the call's
// first. A round snapshot's call round carries its own read's instant, so it
// is not moved by it.
func TestACallsMarkIsMovedByAnotherStagingOrBoundaryOfTheRound(t *testing.T) {
	at := RoundAt{Epoch: 3, Gen: 7, Bounds: 5, Taken: 2}
	marked := NewCallRound(3, nil).ReadAt(at)
	for _, tc := range []struct {
		name  string
		call  CallRound
		later RoundAt
		moved bool
	}{
		{"the same round", marked, at, false},
		{"the same round, more taken since", marked, RoundAt{Epoch: 3, Gen: 7, Bounds: 5, Taken: 9}, false},
		{"another generation alone (a key claimed again)", marked, RoundAt{Epoch: 3, Gen: 8, Bounds: 5, Taken: 2}, false},
		{"another boundary count", marked, RoundAt{Epoch: 3, Gen: 8, Bounds: 6, Taken: 2}, true},
		{"another staging", marked, RoundAt{Epoch: 4, Gen: 7, Bounds: 5}, true},
		{"a mark that keeps no generation", NewCallRound(3, nil), at, true},
		{"a mark that keeps no generation, read at zero", NewCallRound(3, nil), RoundAt{Epoch: 3}, true},
		{"the zero CallRound", CallRound{}, at, false},
		{"a mark at zero, read again there", NewCallRound(3, nil).ReadAt(RoundAt{Epoch: 3}), RoundAt{Epoch: 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.call.MovedSince(tc.later); got != tc.moved {
				t.Fatalf("MovedSince(%+v) = %v; want %v", tc.later, got, tc.moved)
			}
		})
	}
	want := RoundAt{Epoch: 3, Gen: 7, Bounds: 5}
	if got, known := NewCallRound(1, nil).ReadAt(at).InStaging(true).InView(true, true).Round(); !known || got != want {
		t.Fatalf("a mark's round is lost across its other settings: %+v (known %v); want %+v", got, known, want)
	}
	if _, known := NewCallRound(3, nil).Round(); known {
		t.Fatal("a mark of no generation says it knows its round")
	}
	for name, s := range map[string]RoundSnapshot{
		"a test's view":                  {At: at, Phase: RoundInTest, Test: "T1", Open: true},
		"the tracker ahead":              {At: at, Phase: RoundBoundary, Learn: LearnRead{Bound: true, Open: true}},
		"a set the store has not staged": {At: at, Phase: RoundBoundary, Learn: LearnRead{Bound: true, SetAhead: true}},
	} {
		if name == "the tracker ahead" && !s.TrackerAhead() || name == "a set the store has not staged" && !s.SetUnstaged() {
			t.Fatalf("precondition: %s does not read so", name)
		}
		c := s.CallRound()
		if got, known := c.Round(); !known || got != want || c.MovedSince(at) {
			t.Errorf("%s: a round snapshot's call round reads %+v (known %v), moved by its own read %v; want %+v", name, got, known, c.MovedSince(at), want)
		}
	}
}

// A view's sequence of a reusable request walks, in a test with a recorded
// window, the recordings the window holds; with none in it, the one in force
// when the test began (the last before it), else the first; between tests,
// and in a test with no window, every one.
func TestASequenceWalksTheOpenTestsRecordedWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(secs ...int) func(int) time.Time {
		return func(i int) time.Time { return t0.Add(time.Duration(secs[i]) * time.Second) }
	}
	in := func(from, to int) SequenceView {
		return SequenceView{Test: "T", Start: t0.Add(time.Duration(from) * time.Second), End: t0.Add(time.Duration(to) * time.Second)}
	}
	for _, c := range []struct {
		name   string
		v      SequenceView
		secs   []int
		lo, hi int
	}{
		{"between tests", SequenceView{}, []int{1, 2, 3}, 0, 3},
		{"a test with no window", SequenceView{Test: "T"}, []int{1, 2, 3}, 0, 3},
		{"the window's own", in(2, 3), []int{1, 2, 3, 4}, 1, 3},
		{"bounds included", in(1, 4), []int{1, 2, 3, 4}, 0, 4},
		{"none in it: the last before", in(5, 6), []int{1, 2, 7}, 1, 2},
		{"none before or in it: the first", in(0, 0), []int{1, 2}, 0, 1},
		{"none recorded", in(1, 2), nil, 0, 0},
	} {
		lo, hi := c.v.Span(len(c.secs), at(c.secs...))
		if lo != c.lo || hi != c.hi {
			t.Errorf("%s: span [%d, %d); want [%d, %d)", c.name, lo, hi, c.lo, c.hi)
		}
	}
}

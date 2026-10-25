package integrations

import "testing"

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

package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A miss's report carries the round its call was marked in: the staging, the
// round's boundary count where the store keeps one (a mark read at a round,
// ReadAt), and the test whose view the read found (OfTest; none for a pool
// view). A mark of a store that keeps no boundary count carries none, so a
// test's end places its miss by when it was filed. The zero CallRound stamps
// nothing.
func TestAMissReportCarriesTheRoundItsCallWasMarkedIn(t *testing.T) {
	for _, c := range []struct {
		name  string
		call  CallRound
		round bool
		test  string
	}{
		{"read at a round, in a test", NewCallRound(3, nil).ReadAt(RoundAt{Epoch: 3, Gen: 7, Bounds: 10}).InView(true, false).OfTest("o::TestA"), true, "o::TestA"},
		{"read at a round, between tests", NewCallRound(3, nil).ReadAt(RoundAt{Epoch: 3, Gen: 7, Bounds: 10}).InView(false, false), true, ""},
		{"a store that keeps no boundary count", NewCallRound(3, nil).InView(true, false).OfTest("o::TestA"), false, "o::TestA"},
	} {
		r := &models.MockMismatchReport{}
		StampMark(r, c.call)
		if r.Mark == models.RoundUnmarked || r.MarkEpoch != 3 {
			t.Fatalf("%s: the report carries no mark: %+v", c.name, r)
		}
		if r.MarkRound != c.round {
			t.Errorf("%s: the report says its boundary count is known: %v; want %v", c.name, r.MarkRound, c.round)
		}
		if c.round && r.MarkBounds != 10 {
			t.Errorf("%s: the report's boundary count is %d; want 10", c.name, r.MarkBounds)
		}
		if r.MarkedTest != c.test {
			t.Errorf("%s: the report names %q as the test its call was made in; want %q", c.name, r.MarkedTest, c.test)
		}
	}
	// A round snapshot's mark names the test whose view it read, except where
	// the learn tracker is ahead of the store (TrackerAhead): the call is the
	// pool's or the tracker's test's, never that of the test whose view is in
	// force, so its miss names none.
	at := RoundAt{Epoch: 3, Gen: 7, Bounds: 10}
	for _, c := range []struct {
		name  string
		s     RoundSnapshot
		ahead bool
		test  string
	}{
		{"read in a test", RoundSnapshot{At: at, Phase: RoundInTest, Test: "o::TestA", Open: true}, false, "o::TestA"},
		{"read with the tracker ahead of a test's view", RoundSnapshot{At: at, Phase: RoundBoundary, Test: "o::TestA", Learn: LearnRead{Bound: true, Open: true}}, true, ""},
		{"read with the tracker ahead of the pool view", RoundSnapshot{At: at, Phase: RoundBoundary, Learn: LearnRead{Bound: true, Open: true}}, true, ""},
	} {
		if c.s.TrackerAhead() != c.ahead {
			t.Fatalf("precondition: %s reads the tracker ahead: %v", c.name, c.s.TrackerAhead())
		}
		r := &models.MockMismatchReport{}
		StampMark(r, c.s.CallRound())
		if !r.MarkRound || r.MarkBounds != 10 {
			t.Fatalf("%s: the report carries no round: %+v", c.name, r)
		}
		if r.MarkedTest != c.test {
			t.Errorf("%s: the report names %q as the test its call was made in; want %q", c.name, r.MarkedTest, c.test)
		}
	}
	r := &models.MockMismatchReport{}
	StampMark(r, CallRound{})
	if r.Mark != models.RoundUnmarked || r.MarkRound || r.MarkEpoch != 0 || r.MarkBounds != 0 || r.MarkedTest != "" || r.MarkedInTest {
		t.Errorf("the zero CallRound stamped %+v", r)
	}
}

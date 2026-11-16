package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// takenInTheGap consumes the recording called name through a delete door,
// and, once the consume settled as a serve and let the round lock go but
// before what the replay counts of it (OnSettled), installs a view
// (install), then reports whether that view says the recording is spent
// (MockSpent) and whether the store finds it unserved, both read in that
// instant. It fails t unless the door served the recording.
func takenInTheGap(t *testing.T, s *roundStore, name string, install func()) (spent, unserved bool) {
	t.Helper()
	mk := shown(s.snapshot(t), name)
	if mk == nil {
		t.Fatalf("precondition: %s is not shown", name)
	}
	ran := false
	s.mm.OnSettled(func(string) {
		s.mm.OnSettled(nil)
		ran = true
		install()
		spent = s.mm.MockSpent(name)
		unserved = hasUnserved(t, s.mm, models.HTTP, name)
	})
	if !s.mm.DeleteFilteredMock(*mk) || !ran {
		t.Fatalf("the consume of %s was refused (settled as a serve: %v)", name, ran)
	}
	return spent, unserved
}

// Under exact serving a take answered this session in the step that takes
// it, holding the round lock, though what the replay counts of it is
// written only as the lock is let go: a view installed in that instant (the
// next test opening, the last test ending to the pool), which no longer
// says the take is spent, still finds the recording served. A call held
// for a recording no call took yet (a blocking pop) reads it so, and misses
// at once rather than wait for a window change no test may bring, though
// another call took a recording in that instant too. So does a staged
// recording spent for the staging (the pool's, outside every test) with a
// test opening in that instant, which the new view says is spent as the
// old one did.
func TestATakeIsServedBeforeTheReplaysCountOfIt(t *testing.T) {
	for _, tc := range []struct {
		name, take string
		// stillSpent: the view installed says the take is spent, as a
		// spend for the staging is in every view of it.
		stillSpent bool
		before     func(s *roundStore)
		install    func(s *roundStore)
	}{
		{"the next test opens", "own", false, (*roundStore).inTest, func(s *roundStore) {
			s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
		}},
		{"the last test ends", "own", false, (*roundStore).inTest, func(s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		}},
		{"another call takes, then the next test opens", "own", false, (*roundStore).inTest, func(s *roundStore) {
			if !s.mm.DeleteFilteredMock(*s.ms["own2"]) {
				panic("precondition: T1's own2 did not answer")
			}
			s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
		}},
		{"a spend for the staging, then a test opens", "pool", true, func(s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		}, (*roundStore).inTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			tc.before(s)
			spent, unserved := takenInTheGap(t, s, tc.take, func() { tc.install(s) })
			if spent != tc.stillSpent {
				t.Fatalf("precondition: the view installed before the count says %s is spent: %v", tc.take, spent)
			}
			if unserved {
				t.Fatalf("%s, taken, was unserved to a view installed before its count", tc.take)
			}
			if hasUnserved(t, s.mm, models.HTTP, tc.take) {
				t.Fatalf("%s was unserved once counted", tc.take)
			}
		})
	}
}

// A pool view's ahead (the next test's call in flight as its test ended)
// answered as the pool view took it: the replay counts it as the next test
// opens, but the store finds it served at once, and after that test opens.
func TestAPoolViewsAheadIsServedAsItIsTaken(t *testing.T) {
	s := newRoundStore(t)
	s.betweenWithAhead()
	mk := shown(s.snapshot(t), "next")
	if mk == nil {
		t.Fatal("precondition: the ahead next is not shown in the pool view")
	}
	if !s.mm.DeleteFilteredMock(*mk) {
		t.Fatal("the pool view's ahead was refused")
	}
	if hasUnserved(t, s.mm, models.HTTP, "next") {
		t.Fatal("the ahead next, taken, was unserved before the next test opened")
	}
	s.begin()
	s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
	if hasUnserved(t, s.mm, models.HTTP, "next") {
		t.Fatal("the ahead next was unserved once the next test opened")
	}
	if !hasUnserved(t, s.mm, models.HTTP, "next2") {
		t.Fatal("T2's next2, which nothing took, was not unserved")
	}
}

// A new replay session counts none of the takes of the session before it.
func TestANewSessionForgetsThePreviousSessionsTakes(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	if spent, unserved := takenInTheGap(t, s, "own", func() {}); !spent || unserved {
		t.Fatalf("own, taken in its view, is spent %v and unserved %v", spent, unserved)
	}
	s.mm.ResetForReplaySession()
	if !hasUnserved(t, s.mm, models.HTTP, "own") {
		t.Fatal("a new session counts the previous session's take")
	}
}

// A pairing's claim of another test's recording takes nothing: the
// recording stays its owner's to answer, and unserved until it does.
func TestAPairingsClaimLeavesTheRecordingUnserved(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	if !s.mm.ClaimPairing(s.ms["other"]) {
		t.Fatal("precondition: the pairing did not claim T3's other")
	}
	if !hasUnserved(t, s.mm, models.HTTP, "other") {
		t.Fatal("other, claimed by a pairing and answered by no call, was served")
	}
}

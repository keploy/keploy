package proxy

import (
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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
// old one did, and a protocol-phase recording a delete door spent in a test
// for that test's view alone (a handshake), which the next test's view
// shows again: it answered this session all the same.
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
		{"a spend for the view alone, then the next test opens", "hs", false, (*roundStore).inTest, func(s *roundStore) {
			s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
		}},
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

// OnSettled(nil) removes the seam: a consume after it settles as a serve
// and runs no seam, as with none ever set.
func TestOnSettledNilRemovesTheSeam(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	ran := 0
	s.mm.OnSettled(func(string) { ran++ })
	s.mm.OnSettled(nil)
	mk := shown(s.snapshot(t), "own")
	if mk == nil {
		t.Fatal("precondition: own is not shown")
	}
	served, panicked := func() (served bool, panicked any) {
		defer func() { panicked = recover() }()
		return s.mm.DeleteFilteredMock(*mk), nil
	}()
	if panicked != nil {
		t.Fatalf("a consume after OnSettled(nil) panicked: %v", panicked)
	}
	if !served || ran != 0 {
		t.Fatalf("a consume after OnSettled(nil): served %v, removed seam ran %d times", served, ran)
	}
}

// takenInItsStep consumes the recording called name through a delete door
// and reports, read in the step that settles the consume, holding the round
// lock, as its take is written to the take log (onTake): whether the view
// says the recording is spent, and whether the store finds it unserved. It
// fails t unless the door served the recording in a step that logged it.
func takenInItsStep(t *testing.T, s *roundStore, name string) (spent, unserved bool) {
	t.Helper()
	mk := shown(s.snapshot(t), name)
	if mk == nil {
		t.Fatalf("precondition: %s is not shown", name)
	}
	ran := false
	var err error
	s.mm.exact.round.onTake = func(tk integrations.Take) {
		if tk.Mock == nil || tk.Mock.Name != name || ran {
			return
		}
		ran = true
		spent = s.mm.MockSpent(name)
		unserved, err = s.mm.HasUnservedRecording(models.HTTP, named(name))
	}
	defer func() { s.mm.exact.round.onTake = nil }()
	if !s.mm.DeleteFilteredMock(*mk) || !ran {
		t.Fatalf("the consume of %s was refused (its take logged: %v)", name, ran)
	}
	if err != nil {
		t.Fatalf("HasUnservedRecording(%s): %v", name, err)
	}
	return spent, unserved
}

// A take marks its recording spent in the view a moment before it notes it
// taken, both in the step that settles it, holding the round lock. A call
// that read the recording spent in that instant (a Redis blocking pop held
// for a recording no call took yet, which leaves out what its read shows)
// finds it answered, not unserved: else it stays held for a window change
// no test may bring, though another call took the recording. So for each
// door's take: a test's own, a pool view's ahead, and a spend for the
// staging outside every test and in a test.
func TestATakeSpentInItsStepIsNeverUnserved(t *testing.T) {
	for _, tc := range []struct {
		name, take string
		before     func(s *roundStore)
	}{
		{"a test's own", "own", (*roundStore).inTest},
		{"a pool view's ahead", "next", (*roundStore).betweenWithAhead},
		{"a spend for the staging outside every test", "pool", func(s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		}},
		{"a spend for the staging in a test", "pool", func(s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
			s.inTest()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			tc.before(s)
			spent, unserved := takenInItsStep(t, s, tc.take)
			if !spent {
				t.Fatalf("precondition: the step logged the take of %s before its view said it is spent", tc.take)
			}
			if unserved {
				t.Fatalf("%s, which its view says is spent, was unserved in the step that takes it", tc.take)
			}
			if hasUnserved(t, s.mm, models.HTTP, tc.take) {
				t.Fatalf("%s was unserved once its take ended", tc.take)
			}
		})
	}
}

// A call that read a recording spent, then asks while the take that spent
// it is still in its step, finds it answered: the store reads the view
// first, which says the recording is spent, and only then the take's note.
// The row pins that order. A store that read the note first (not written
// yet) and the view after would let the step end and the next test open
// between the two reads, with a view that no longer says the recording is
// spent, and both reads would miss it: the onRead seam opens the next test
// there. Read in this order the view answers, and the seam never runs.
func TestATakeIsAnsweredToACallThatAsksAsTheNextTestOpens(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	mk := shown(s.snapshot(t), "own")
	if mk == nil {
		t.Fatal("precondition: own is not shown")
	}
	inStep, release, took := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	s.mm.exact.round.onTake = func(tk integrations.Take) {
		if tk.Mock != nil && tk.Mock.Name == "own" {
			close(inStep)
			<-release
		}
	}
	opened := false
	s.mm.exact.round.onRead = func(step string) {
		if step != "answered own" || opened {
			return
		}
		opened = true
		close(release)
		if !<-took {
			panic("precondition: the consume of own was refused")
		}
		s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
	}
	t.Cleanup(func() { s.mm.exact.round.onTake, s.mm.exact.round.onRead = nil, nil })
	go func() { took <- s.mm.DeleteFilteredMock(*mk) }()
	<-inStep
	if !s.mm.MockSpent("own") {
		t.Fatal("precondition: own is not spent in the step that takes it")
	}
	unserved := hasUnserved(t, s.mm, models.HTTP, "own")
	if !opened {
		close(release)
		if !<-took {
			t.Fatal("the consume of own was refused")
		}
	}
	if unserved {
		t.Fatalf("own, read spent in the step that takes it, was unserved to a call that asked as the next test opened (the test opened in the ask: %v)", opened)
	}
	if hasUnserved(t, s.mm, models.HTTP, "own") {
		t.Fatal("own was unserved once its take ended")
	}
}

// onDisk keeps every recording of the set, and extra, on disk as the agent
// does until their tests open (SetRecordedOfKind): the store reads there one
// the trees do not hold.
func (s *roundStore) onDisk(t *testing.T, extra ...*models.Mock) {
	t.Helper()
	all := append([]*models.Mock(nil), extra...)
	for _, m := range s.ms {
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	s.mm.SetRecordedOfKind(diskOf(t, all...).RangeKind)
}

// restageWithout stages the set again without the recording called name,
// as the replay stages the next window: the trees no longer hold it.
func (s *roundStore) restageWithout(name string) {
	var rest []*models.Mock
	for _, m := range s.mm.StagedMocks() {
		if m.Name != name {
			rest = append(rest, m)
		}
	}
	s.mm.SetMocksWithWindow(nil, rest, models.BaseTime, time.Now())
}

// A pool view's ahead the trees do not hold (one the agent keeps on disk
// until its test opens, handed to the pool view as the next test's call in
// flight) answered as the pool view took it: the store, which reads it on
// disk with no view to say it is spent, finds it served from the take on,
// and once the test it was answered for opens and the replay counts it.
// Answered for a test that did not open next (the next view voids it), it
// answered no request the replay counts, and is unserved again, as it was
// before the take.
func TestAPoolViewsAheadOffTheTreesIsServedAsItIsTaken(t *testing.T) {
	for _, tc := range []struct {
		name, open string
		own        []string
		unserved   bool
	}{
		{"the test it was answered for opens", "T2", []string{"later", "next2"}, false},
		{"another test opens instead", "T3", []string{"other"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			later := httpCall("later", 9*time.Second, "/later")
			s.onDisk(t, later)
			s.ms["later"] = later
			if !hasUnserved(t, s.mm, models.HTTP, "later") {
				t.Fatal("precondition: later, kept on disk and taken by no call, is not unserved")
			}
			s.inTest()
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, later)
			s.end()
			if roundMock(s.mm, "later") != nil {
				t.Fatal("precondition: the trees hold later")
			}
			mk := shown(s.snapshot(t), "later")
			if mk == nil {
				t.Fatal("precondition: the ahead later is not shown in the pool view")
			}
			if !s.mm.DeleteFilteredMock(*mk) {
				t.Fatal("the pool view's ahead later was refused")
			}
			if hasUnserved(t, s.mm, models.HTTP, "later") {
				t.Fatal("the ahead later, taken off the trees, was unserved before the next test opened")
			}
			var own []*models.Mock
			for _, n := range tc.own {
				own = append(own, s.ms[n])
			}
			s.begin()
			s.mm.ServeExactTest(tc.open, own)
			if got := hasUnserved(t, s.mm, models.HTTP, "later"); got != tc.unserved {
				t.Fatalf("the ahead later, taken, is unserved %v once %s opened; want %v", got, tc.open, tc.unserved)
			}
		})
	}
}

// A take whose recording a restage moves off the trees in the instant
// before the replay counts it (the next window staged, as the replay stages
// it) is read on disk, with no view to say it is spent: the store finds it
// served from the take on, a test's own as a pool view's ahead, whose count
// comes only as the next test opens.
func TestATakeRestagedOffTheTreesBeforeTheReplaysCountOfItIsServed(t *testing.T) {
	for _, tc := range []struct {
		name, take string
		before     func(s *roundStore)
	}{
		{"a test's own", "own", (*roundStore).inTest},
		{"a pool view's ahead", "next", (*roundStore).betweenWithAhead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			s.onDisk(t)
			tc.before(s)
			inTrees := true
			_, unserved := takenInTheGap(t, s, tc.take, func() {
				s.restageWithout(tc.take)
				inTrees = roundMock(s.mm, tc.take) != nil
			})
			if inTrees {
				t.Fatalf("precondition: the trees still hold %s once staged again without it", tc.take)
			}
			if unserved {
				t.Fatalf("%s, taken, was unserved once a restage before its count moved it off the trees", tc.take)
			}
			if hasUnserved(t, s.mm, models.HTTP, tc.take) {
				t.Fatalf("%s was unserved off the trees once its take ended", tc.take)
			}
		})
	}
}

// A take given back in the instant before the replay counts it (its test
// opened again, a retry, or the staging's spend restarted with its key)
// answered all the same: the replay's count of it comes as that instant
// ends and is never taken back. So the store finds it served from the take
// on, though the view in force shows it unspent again.
func TestATakeGivenBackBeforeTheReplaysCountOfItAnswered(t *testing.T) {
	for _, tc := range []struct {
		name, take       string
		before, giveBack func(s *roundStore)
	}{
		{"its test opens again", "own", (*roundStore).inTest, func(s *roundStore) {
			s.mm.ServeExactTest("T1", []*models.Mock{s.ms["own"], s.ms["own2"]})
		}},
		{"the staging's spend restarts with its key", "pool", func(s *roundStore) {
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		}, func(s *roundStore) {
			s.mm.RestartExactKey(models.PoolKeyNone, []string{"pool"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			tc.before(s)
			spent, unserved := takenInTheGap(t, s, tc.take, func() { tc.giveBack(s) })
			if spent {
				t.Fatalf("precondition: %s, given back, is still spent", tc.take)
			}
			if unserved {
				t.Fatalf("%s, taken and given back before its count, was unserved", tc.take)
			}
			if hasUnserved(t, s.mm, models.HTTP, tc.take) {
				t.Fatalf("%s, given back, was unserved once counted", tc.take)
			}
		})
	}
}

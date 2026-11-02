package proxy

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A call's mark says when it holds every recording because the store could
// not tell which are the call's (integrations.CallRound.HoldsEveryRecording):
// the learn tracker ahead of the store, whether the store shows its pool view
// or the view of the test before, and a test set the store has not staged.
// Held is true of every name there, so it tells a parser nothing of whose a
// recording is: a parser that advised the user to mark a recording's fields
// as the call's noise by Held alone would advise it of the ended test's
// recording, or of the set's background, in the gap. A mark the store could
// tell (a test's view, the view a test the tracker still has open ended in)
// says it could, through the store's mark, a scoped view's and a round
// snapshot's alike.
func TestAMarkSaysWhenItHoldsEveryRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	marks := map[string]func(mm *MockManager, tr *learn.Tracker) integrations.CallRound{
		"the store's mark": func(mm *MockManager, _ *learn.Tracker) integrations.CallRound { return mm.MarkCallRound() },
		"a scoped view's mark": func(mm *MockManager, _ *learn.Tracker) integrations.CallRound {
			return integrations.MarkCallRound(&scopedMockDb{MockMemDb: mm})
		},
		"a round snapshot": func(mm *MockManager, tr *learn.Tracker) integrations.CallRound {
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
			if !ok {
				t.Fatal("no snapshot")
			}
			return snap.CallRound()
		},
	}
	begin := func(t *learn.Tracker) { t.Begin() }
	end := func(t *learn.Tracker) { t.End() }
	for _, tc := range []struct {
		name  string
		round func(mm *MockManager, tr *learn.Tracker, own *models.Mock)
		every bool
	}{
		{"in a test's view", func(mm *MockManager, tr *learn.Tracker, own *models.Mock) {
			mm.LearnBoundary(tr, begin)
			mm.ServeExactTest("T1", []*models.Mock{own})
		}, false},
		{"where the store ended the test the tracker still has open", func(mm *MockManager, tr *learn.Tracker, own *models.Mock) {
			mm.LearnBoundary(tr, begin)
			mm.ServeExactTest("T1", []*models.Mock{own})
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}, false},
		{"where the tracker is ahead of the store's pool view", func(mm *MockManager, tr *learn.Tracker, own *models.Mock) {
			mm.LearnBoundary(tr, begin)
			mm.ServeExactTest("T1", []*models.Mock{own})
			mm.LearnBoundary(tr, end)
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.LearnBoundary(tr, begin)
		}, true},
		{"where the tracker is ahead of the view of the test before", func(mm *MockManager, tr *learn.Tracker, own *models.Mock) {
			mm.LearnBoundary(tr, begin)
			mm.ServeExactTest("T1", []*models.Mock{own})
			mm.LearnBoundary(tr, end)
			mm.LearnBoundary(tr, begin)
		}, true},
		{"in a test set the store has not staged", func(mm *MockManager, tr *learn.Tracker, own *models.Mock) {
			mm.LearnBoundary(tr, begin)
			mm.ServeExactTest("T1", []*models.Mock{own})
			mm.LearnBoundary(tr, end)
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.LearnBoundary(tr, func(t *learn.Tracker) { t.NewSet() })
		}, true},
	} {
		for how, mark := range marks {
			t.Run(how+" "+tc.name, func(t *testing.T) {
				own := genericExchange("ex-own", 0, "AUTH step2")
				other := genericExchange("ex-other", 0, "AUTH step3")
				mm := stagedManager(t, nil, []*models.Mock{own, other})
				tr := learn.NewTracker()
				tc.round(mm, tr, own)
				c := mark(mm, tr)
				if !c.Read() {
					t.Fatal("precondition: the call was not marked")
				}
				if got := c.HoldsEveryRecording(); got != tc.every {
					t.Fatalf("the mark holds every recording: %v; want %v", got, tc.every)
				}
				if c.Held(other.Name) != tc.every {
					t.Fatalf("the mark holds ex-other, no view's own: %v; want %v", c.Held(other.Name), tc.every)
				}
			})
		}
	}
	if (integrations.CallRound{}).HoldsEveryRecording() {
		t.Fatal("the zero CallRound, of no read, holds every recording")
	}
}

// No flow keploy runs installs a test's view while the learn tracker still
// has another test open (integrations.RoundSnapshot.Ended with a test's view
// in force): a mark read there holds both tests' recordings as the call's own
// and says it could tell (HoldsEveryRecording false), so a parser's advice
// for the call's closest recording could name either test's. Each flow below
// is the order its caller keeps, pinned there: keploy test's set loop (also
// keploy test --judge's and keploy record's own rounds, which run it) serves
// the pool, opens the test's capture window, where the tracker begins the
// test, and only then asks for its view, and a window it cannot open stops
// the set (replay's TestRunTestSetOpensEachTestsCaptureBeforeMovingItsMockWindow,
// TestATestCaseTheAgentCannotBeginStopsTheSetBeforeItsViewIsServed); keploy
// mock replay's begin opens the window before it installs the view, and its
// end serves the pool before the read that ends the tracker's test (agent's
// TestABeginOpensItsCaptureWindowBeforeItsView); keploy mock record installs
// no view. A begin always moves the tracker to a test of its own, so a view
// installed after it is that test's, whether or not the previous test's end
// reached the tracker. The last case is the order no flow keeps, a view
// installed with no begin of its own: it reaches the state, so the check
// above it can fail.
func TestNoFlowShowsATestsViewWhileTheTrackerHasAnotherTestOpen(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	type step struct {
		what string
		do   func(mm *MockManager, tr *learn.Tracker, own map[string]*models.Mock)
	}
	boundary := func(what string, f func(*learn.Tracker)) step {
		return step{what, func(mm *MockManager, tr *learn.Tracker, _ map[string]*models.Mock) { mm.LearnBoundary(tr, f) }}
	}
	begin := boundary("the tracker begins", func(t *learn.Tracker) { t.Begin() })
	awaiting := boundary("the tracker begins, awaiting the request", func(t *learn.Tracker) { t.BeginAwaiting() })
	end := boundary("the tracker ends", func(t *learn.Tracker) { t.End() })
	pool := step{"the pool view", func(mm *MockManager, _ *learn.Tracker, _ map[string]*models.Mock) {
		mm.ServeExactPool(models.PoolKeyNone, nil)
	}}
	open := func(test string) step {
		return step{test + "'s view", func(mm *MockManager, _ *learn.Tracker, own map[string]*models.Mock) {
			mm.OpenExactTest(test, []*models.Mock{own[test]}, nil)
		}}
	}
	concurrent := func(test, with string) step {
		return step{test + "'s concurrent view", func(mm *MockManager, _ *learn.Tracker, own map[string]*models.Mock) {
			mm.OpenExactTestConcurrent(test, []*models.Mock{own[test]}, []*models.Mock{own[with]}, nil)
		}}
	}
	join := func(parts ...[]step) []step {
		var out []step
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	// keploy test's set loop: poolBeforeBegin, openTestErrorCapture (a
	// keploy test case's window awaits its request), serveExactTest, and the
	// test's GetMockErrors, which ends the tracker's test.
	loop := func(test string) []step { return []step{pool, awaiting, open(test), end} }
	// keploy mock replay: ProvenBegin (the window, then serveTestExactly),
	// ProvenEnd (servePoolExactly, then testEvidence's GetMockErrors).
	mockReplay := func(test string) []step { return []step{begin, open(test), pool, end} }
	for _, tc := range []struct {
		name    string
		steps   []step
		reached bool
		// noView: the flow installs no view, so no call is marked.
		noView bool
	}{
		{"keploy test's set loop", join(loop("T1"), loop("T2"), loop("T3")), false, false},
		{"keploy test's set loop where a test's end never reached the tracker",
			join(loop("T1"), []step{pool, awaiting, open("T2")}, loop("T3")), false, false},
		{"keploy record's own round, serving concurrent tests",
			[]step{pool, awaiting, concurrent("T1", "T2"), end, pool, awaiting, concurrent("T2", "T1"), end}, false, false},
		{"keploy mock replay", join(mockReplay("T1"), mockReplay("T2"), mockReplay("T3")), false, false},
		{"keploy mock replay where a begin's view was refused",
			join(mockReplay("T1"), []step{begin, end}, mockReplay("T3")), false, false},
		{"keploy mock replay where an end's read never reached the tracker",
			join([]step{begin, open("T1"), pool}, mockReplay("T2")), false, false},
		{"keploy mock replay where a begin came before the run's table",
			join([]step{begin, pool, open("T1"), pool, end}, mockReplay("T2")), false, false},
		{"keploy mock record, which installs no view", []step{begin, end, begin, end}, false, true},
		{"a view installed with no begin of its own", []step{begin, open("T1"), open("T2")}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own := map[string]*models.Mock{
				"T1": genericExchange("ex-t1", 0, "AUTH step1"),
				"T2": genericExchange("ex-t2", 0, "AUTH step2"),
				"T3": genericExchange("ex-t3", 0, "AUTH step3"),
			}
			mm := stagedManager(t, nil, []*models.Mock{own["T1"], own["T2"], own["T3"]})
			tr := learn.NewTracker()
			reached := ""
			for i, s := range tc.steps {
				s.do(mm, tr, own)
				if tc.noView && mm.MarkCallRound().Read() {
					t.Fatalf("after step %d (%s) a call is marked in a flow that installs no view", i, s.what)
				}
				snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
				if !ok || snap.Test == "" || snap.Ended == "" || snap.Ended == snap.Test {
					continue
				}
				c := mm.MarkCallRound()
				if !c.InTest() || c.HoldsEveryRecording() || !c.Held(own[snap.Ended].Name) || !c.Held(own[snap.Test].Name) {
					t.Fatalf("after step %d (%s) the store shows %s's view with the tracker's open test %s's, but the mark (in test %v, holds every %v) does not hold both tests' own", i, s.what, snap.Test, snap.Ended, c.InTest(), c.HoldsEveryRecording())
				}
				if reached == "" {
					reached = fmt.Sprintf("after step %d (%s): the store shows %s's view, the tracker's open test is %s's", i, s.what, snap.Test, snap.Ended)
				}
			}
			if got := reached != ""; got != tc.reached {
				t.Fatalf("the flow shows a test's view with another test open in the tracker: %v (%s); want %v", got, reached, tc.reached)
			}
		})
	}
}

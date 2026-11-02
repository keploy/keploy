package proxy

import (
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

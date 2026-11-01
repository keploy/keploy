package proxy

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// A parser reaches the store's learning-only pairing through the views it is
// handed, never the manager itself: the scoped view of its connection
// (scopedMockDb) and the store held to its call's round (HoldToRound, bare or
// under the scoped view). Each view passes the pairing on: the candidates
// are the manager's, and what a call teaches through the view is the
// manager's to give record's full round (LearnedElsewhere), which the proxy
// forwards to the agent. A view that dropped either would turn the learning
// off with every parser row still green, since those rows hand the parser a
// store of their own.
func TestTheViewsAParserReadsPassOnLearningAcrossWindows(t *testing.T) {
	minute := map[string]models.ClockOffset{"claimed_at": {Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}}
	open := func(t *testing.T) (*MockManager, map[string]*models.Mock) {
		t.Helper()
		mm, ms := s12Set(t)
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
		mm.SetPairing(true)
		mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
		return mm, ms
	}
	learnedOther := func(t *testing.T, mm *MockManager, through string) {
		t.Helper()
		got := mm.LearnedElsewhere()
		if len(got) != 1 || got[0].Name != "other" || !reflect.DeepEqual(got[0].Clocks, minute) {
			t.Fatalf("taught through %s, the manager keeps %+v; want other's claimed_at", through, got)
		}
	}

	for _, c := range []struct {
		name string
		view func(mm *MockManager) integrations.MockMemDb
	}{
		{"the scoped view", func(mm *MockManager) integrations.MockMemDb { return &scopedMockDb{MockMemDb: mm} }},
		{"the store held to the call's round", func(mm *MockManager) integrations.MockMemDb {
			held, _ := mm.HoldToRound(mm.MarkCallRound())
			return held
		}},
		{"the scoped view held to the call's round", func(mm *MockManager) integrations.MockMemDb {
			scoped := &scopedMockDb{MockMemDb: mm}
			held, _ := scoped.HoldToRound(scoped.MarkCallRound())
			return held
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm, ms := open(t)
			db := c.view(mm)
			equalNames(t, "the candidates through "+c.name, integrations.LearnElsewhereCandidates(db, models.HTTP, nil), "other")
			if !integrations.LearnElsewhereOnto(db, ms["other"], minute) {
				t.Fatalf("a call taught other nothing through %s", c.name)
			}
			learnedOther(t, mm, c.name)
			integrations.CountLearnElsewhere(db, models.Mongo, "insert events in keploy", "learned its clock reads")
			if got := mm.elsewhereCounts[elsewhereOp{models.Mongo, "insert events in keploy"}]["learned its clock reads"]; got != 1 {
				t.Fatalf("a count made through %s reached the manager %d times; want once", c.name, got)
			}
		})
	}

	// A call held to its round teaches nothing once the test set was staged
	// again under it: names restart in each staging, so the recording it
	// would teach is a later set's that shares a name. The manager itself,
	// read with no hold, still learns: the hold is what refuses.
	t.Run("restaged under the held call", func(t *testing.T) {
		mm, ms := open(t)
		held, restaged := mm.HoldToRound(mm.MarkCallRound())
		mm.bumpStagingEpoch()
		if integrations.LearnElsewhereOnto(held, ms["other"], minute) || !restaged() {
			t.Fatalf("a call held to an earlier staging taught a recording of a later one (restaged %v)", restaged())
		}
		if got := mm.LearnedElsewhere(); len(got) != 0 {
			t.Fatalf("a call held to an earlier staging left %+v learned", got)
		}
		if !mm.LearnElsewhere(ms["other"], minute) {
			t.Fatal("precondition: with no hold, the manager taught other nothing in the new staging")
		}
	})

	// The proxy forwards the manager's whole: the agent's read
	// (/mock/learned-elsewhere and the compose outcome) asks the proxy.
	t.Run("the proxy", func(t *testing.T) {
		mm, ms := open(t)
		if !mm.LearnElsewhere(ms["other"], minute) {
			t.Fatal("precondition: a call taught other nothing")
		}
		p := &Proxy{mockManager: mm}
		if got, want := p.LearnedElsewhere(), mm.LearnedElsewhere(); len(want) != 1 || !reflect.DeepEqual(got, want) {
			t.Fatalf("the proxy says %+v was learned across windows; want the manager's %+v", got, want)
		}
		if got := (&Proxy{}).LearnedElsewhere(); got != nil {
			t.Fatalf("a proxy with no manager says %+v was learned", got)
		}
	})
}

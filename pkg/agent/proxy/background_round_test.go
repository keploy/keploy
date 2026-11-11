package proxy

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A call's miss is judged by the background recordings of the set it was
// marked in, not of the set staged when keploy decides it: the replay can
// stage the next set first. The install before the current one is kept for
// that; a call marked before both reads none (it counts), the zero
// CallRound reads the current set's, a restage within a set keeps its
// recordings, and a seed within one staging replaces that staging's alone.
func TestACallIsJudgedByTheBackgroundRecordingsOfTheSetItWasMarkedIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mark := func() integrations.CallRound { return integrations.NewCallRound(mm.StagingEpoch(), nil) }
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	reads := func(what string, call integrations.CallRound, want ...string) {
		t.Helper()
		if got := bgNames(mm.BackgroundRecordingsIn(call, models.Mongo)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the manager reads %v; want %v", what, got, want)
		}
		if got := bgNames(integrations.BackgroundRecordingsAt(view, call, models.Mongo)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: a test's view reads %v; want %v", what, got, want)
		}
	}

	// Set A.
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("a-poll", models.Mongo, 1)})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	a := mark()
	reads("a call of set A", a, "a-poll")
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now()) // a restage within A
	if mm.StagingEpoch() == a.Epoch() {
		t.Fatal("the restage started no staging epoch (this case's premise)")
	}
	reads("a call of set A marked before its restage", a, "a-poll")
	reads("a call of set A marked after its restage", mark(), "a-poll")

	// Set B is seeded: until it is staged, A's are in force.
	mm.ResetForReplaySession()
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("b-poll", models.Mongo, 2)})
	reads("a call of set A, B seeded and not staged", a, "a-poll")
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	b := mark()
	reads("a call of set A decided after B's staging", a, "a-poll")
	reads("a call of set B", b, "b-poll")
	reads("a call of no mark", integrations.CallRound{}, "b-poll")
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"b-poll"}) {
		t.Fatalf("the current set's are %v; want [b-poll]", got)
	}

	// Set C seeds none: B's call keeps B's, A's (two sets back) reads none.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	c := mark()
	reads("a call of set C, which seeded none", c)
	reads("a call of set B decided after C's staging", b, "b-poll")
	reads("a call of set A, two sets back", a)

	// A seed within C's staging replaces C's there, and keeps B's.
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("c-poll", models.Mongo, 3)})
	reads("a call of set C after its seed", c, "c-poll")
	reads("a call of set B after C's seed", b, "b-poll")
	// And one in a later staging of C keeps C's first for the calls before.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("c2-poll", models.Mongo, 4)})
	reads("a call of set C before its second seed", c, "c-poll")
	reads("a call of set C after its second seed", mark(), "c2-poll")
	reads("a call of set B after C's second seed", b)
}

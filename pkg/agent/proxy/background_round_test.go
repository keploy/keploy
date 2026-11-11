package proxy

import (
	"fmt"
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

// A call's background miss reads the bound values of the set it was marked
// in, as its background recordings: what that set's tests bound and the
// test each recording trails by that set's scope table, not the next set's,
// which the replay can begin first and which forgets every binding of the
// set before. The set before the current one is kept; a call marked before
// both reads none (its background miss counts); the zero CallRound and a
// call of the current set read the current set's; a test's view forwards
// it; and a new set begun again before its staging keeps the set before.
func TestACallReadsTheBoundValuesOfTheSetItWasMarkedIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mark := func() integrations.CallRound { return integrations.NewCallRound(mm.StagingEpoch(), nil) }
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	bind := func(name, value, mock string) models.VariableBinding {
		return models.VariableBinding{Name: name, Value: value, Recorded: "rec-" + name, Mock: mock, Place: "body.id"}
	}
	type read struct{ own, ended, trail, trailMocks string }
	text := func(bv integrations.BoundValues, endedTest, mock string) *read {
		if bv == nil {
			return nil
		}
		names := func(bs []models.VariableBinding) string {
			out := ""
			for _, b := range bs {
				out += b.Name + "=" + b.Value + ";"
			}
			return out
		}
		tr := bv.TrailOf(mock)
		return &read{own: names(bv.Own()), ended: names(bv.EndedOf(endedTest)), trail: tr.Test, trailMocks: fmt.Sprint(tr.Mocks)}
	}
	reads := func(what string, call integrations.CallRound, endedTest, mock string, want *read) {
		t.Helper()
		if got := text(mm.BoundValuesIn(call), endedTest, mock); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the manager reads %+v; want %+v", what, got, want)
		}
		if got := text(integrations.BoundValuesAt(view, call), endedTest, mock); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: a test's view reads %+v; want %+v", what, got, want)
		}
	}

	// Set A: test-a1 bound app=a1 at mock-1 and ended; test-a2 bound
	// own=a2. mock-9, a worker: recording, trails test-a1.
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	mm.SetPoolTests(map[string][]string{"test-a1": {"mock-1"}, "test-a2": {"mock-2"}})
	mm.SetPoolTrails(map[string]string{"mock-9": "test-a1"})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.Variables().StartOpeningTest("test-a1", func() {}, nil, nil, nil)
	mm.Variables().Bind(bind("app", "a1", "mock-1"))
	mm.Variables().StartOpeningTest("test-a2", func() {}, nil, nil, nil)
	mm.Variables().Bind(bind("own", "a2", "mock-2"))
	a := mark()
	setA := &read{own: "own=a2;", ended: "app=a1;", trail: "test-a1", trailMocks: "[mock-1]"}
	reads("a call of set A", a, "test-a1", "mock-9", setA)

	// Set B begins (twice: the replay's re-registration before staging),
	// with its own scope table, which trails nothing; staged, its test-b1
	// binds own=b1.
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	reads("a call of set A, B begun and not staged", a, "test-a1", "mock-9", setA)
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	reads("a call of set A, B begun again and not staged", a, "test-a1", "mock-9", setA)
	mm.SetPoolTests(map[string][]string{"test-b1": {"mock-1"}})
	mm.SetPoolTrails(map[string]string{})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if mm.StagingEpoch() == a.Epoch() {
		t.Fatal("set B's staging started no staging epoch (this case's premise)")
	}
	mm.Variables().StartOpeningTest("test-b1", func() {}, nil, nil, nil)
	mm.Variables().Bind(bind("own", "b1", "mock-1"))
	b := mark()
	setB := &read{own: "own=b1;", trailMocks: "[]"}
	reads("a call of set A decided in set B", a, "test-a1", "mock-9", setA)
	reads("a call of set B", b, "test-a1", "mock-9", setB)
	reads("a call of no mark", integrations.CallRound{}, "test-a1", "mock-9", setB)

	// Set C: B's call reads B's as B ended; A's, two sets back, none.
	mm.ResetForReplaySession()
	mm.NewVariableSet()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	reads("a call of set B decided in set C", b, "test-a1", "mock-9", setB)
	reads("a call of set A, two sets back", a, "test-a1", "mock-9", nil)
	reads("a call of set C", mark(), "test-a1", "mock-9", &read{trailMocks: "[]"})
}

package replay

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, a test whose only misses are background misses
// (the same request as one of the set's background recordings) passes; one
// of its own misses beside them still fails it. A background miss made before
// the set's first test fails no set either.
func TestABackgroundMissFailsNoTestAndNoSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	poll := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "findAndModify ptw_endpoints", MatchPhase: models.MatchPhaseBody,
		Kind: models.MissExtraCall, Recorded: 1, Background: true}
	own := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "find users", MatchPhase: models.MatchPhaseBody, Kind: models.MissInTest}
	results := []models.TestResult{
		{TestCaseID: "only-background", Status: models.TestStatusPassed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{poll, poll}}},
		{TestCaseID: "own-miss", Status: models.TestStatusPassed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{poll, own}}},
	}
	if failed := failTestsWithMisses(results); !reflect.DeepEqual(failed, []string{"own-miss"}) {
		t.Fatalf("failTestsWithMisses failed %v; want only the test with its own miss", failed)
	}
	if results[0].Status != models.TestStatusPassed || results[1].Status != models.TestStatusFailed {
		t.Fatalf("statuses %s, %s; want PASSED, FAILED", results[0].Status, results[1].Status)
	}
	if n := len(results[0].FailureInfo.UnmatchedCalls); n != 2 {
		t.Fatalf("the passing test lists %d background misses; want both kept, listed", n)
	}

	var set setCalls
	before := poll
	before.Kind, before.Recorded, before.BeforeSet = models.MissOutsideTest, 0, true
	ownBefore := own
	ownBefore.Kind, ownBefore.BeforeSet = models.MissOutsideTest, true
	if rest := set.take([]models.UnmatchedCall{before}); len(rest) != 0 {
		t.Fatalf("the set's background call stayed with the test: %+v", rest)
	}
	if f := set.failing(); len(f) != 0 {
		t.Fatalf("a background call before the set's first test fails the set: %+v", f)
	}
	set.take([]models.UnmatchedCall{ownBefore})
	if f := set.failing(); len(f) != 1 || f[0].Background {
		t.Fatalf("the set's own call before its first test: failing %+v; want it alone", f)
	}
}

// The run's mismatch store keeps the mark of a miss the agent read, so a
// green run's table leaves a background miss out as it leaves out every miss
// that counts against no test.
func TestTheMismatchStoreKeepsTheBackgroundMark(t *testing.T) {
	s := NewTestFailureStore()
	s.AddUnmatchedCallForTest("set-0", "test-1", models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "findAndModify ptw_endpoints", Background: true})
	s.AddUnmatchedCallForTest("set-0", "test-1", models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "find users"})
	got := s.GetFailures()
	if len(got) != 2 || !got[0].MismatchReport.Background || got[0].MismatchReport.CountsAgainstTest() ||
		got[1].MismatchReport.Background || !got[1].MismatchReport.CountsAgainstTest() {
		t.Fatalf("the store's reports lost or invented the mark: %+v, %+v", got[0].MismatchReport, got[1].MismatchReport)
	}
}

// The set's staging call names its background recordings, its mappings.yaml
// worker: and startup: mocks, and no per-test call does. A mock a
// worker-mock rule marked in the worker scope (models.WorkerRuleReason("jwks"))
// is not one: keploy test serves it to every test as a session mock
// (models.ServeAsSession), not as a background recording.
func TestTheStagingCallNamesTheSetsBackgroundRecordings(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.mappingDB = &shMappingDB{prMappingDB: &prMappingDB{}, mapping: &models.Mapping{
		Worker:  []models.MockEntry{{Name: "poll-1"}, {Name: "jwks-1", Reason: models.WorkerRuleReason("jwks")}, {Name: "poll-2"}},
		Startup: []models.MockEntry{{Name: "boot-1"}},
	}}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED", status)
	}
	h.instr.mu.Lock()
	params := append([]models.MockFilterParams(nil), h.instr.allParams...)
	h.instr.mu.Unlock()
	staged := 0
	for _, p := range params {
		if !p.AfterTime.Equal(models.BaseTime) {
			if len(p.BackgroundMocks) != 0 {
				t.Fatalf("a per-test call named background recordings %v; they belong to the staging call", p.BackgroundMocks)
			}
			continue
		}
		staged++
		if want := []string{"poll-1", "poll-2", "boot-1"}; !reflect.DeepEqual(p.BackgroundMocks, want) {
			t.Fatalf("the staging call named %v; want %v", p.BackgroundMocks, want)
		}
	}
	if staged != 1 {
		t.Fatalf("%d staging calls, want 1", staged)
	}
}

// A run's prune keeps the set's background recordings, consumed or not: no test
// owns one, so nothing else keeps it, and the next run reads a call's miss
// against it. Neither section's mock leaves, and a name of neither is not kept.
func TestThePruneKeepsTheSetsBackgroundRecordings(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.mappingDB = &shMappingDB{prMappingDB: &prMappingDB{}, mapping: &models.Mapping{
		Worker:  []models.MockEntry{{Name: "poll-1"}},
		Startup: []models.MockEntry{{Name: "boot-1"}},
	}}
	h.replayer.config.Test.RemoveUnusedMocks = true
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED", status)
	}
	if got := h.mocks.pruneCalls(); got != 1 {
		t.Fatalf("%d prunes, want 1", got)
	}
	kept := h.mocks.kept
	if _, ok := kept["poll-1"]; !ok {
		t.Fatalf("the prune's keep-set %v drops the worker: recording", kept)
	}
	if _, ok := kept["boot-1"]; !ok {
		t.Fatalf("the prune's keep-set %v drops the startup: recording", kept)
	}
	if len(kept) != 2 {
		t.Fatalf("the prune's keep-set %v holds more than the set's background recordings", kept)
	}
}

// A background recording a passing test consumed keeps what that run learned:
// keeping it does not replace its state.
func TestThePruneKeepsAConsumedBackgroundRecordingsState(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.mappingDB = &shMappingDB{prMappingDB: &prMappingDB{}, mapping: &models.Mapping{
		Worker: []models.MockEntry{{Name: "poll-1"}},
	}}
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: "poll-1", ReqBodyNoise: map[string][]string{"filter.at": {}}}}}
	h.replayer.config.Test.RemoveUnusedMocks = true
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED", status)
	}
	if got := h.mocks.kept["poll-1"]; len(got.ReqBodyNoise) != 1 {
		t.Fatalf("the prune's keep-set holds %+v for the consumed recording; its learned noise is gone", got)
	}
}

// The background recordings are the file's worker: and startup: mocks; under
// the proven switch the worker scope is the one the run serves (a stale move
// returned to its test is that test's again). A store that reads only the
// startup section names those; one that cannot read the file names none.
func TestTheBackgroundRecordingsAreTheWorkerScopeTheRunServes(t *testing.T) {
	mapping := &models.Mapping{Worker: []models.MockEntry{{Name: "poll-1"}, {Name: "moved-back"}}, Startup: []models.MockEntry{{Name: "boot-1"}}}
	r := newReplayerWithMappingDB(&shMappingDB{prMappingDB: &prMappingDB{}, mapping: mapping})
	if got, want := r.backgroundMockNames(context.Background(), "s"), []string{"poll-1", "moved-back", "boot-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch off: %v, want %v", got, want)
	}
	r.exact = &exactPlan{worker: []models.MockEntry{{Name: "poll-1"}}}
	if got, want := r.backgroundMockNames(context.Background(), "s"), []string{"poll-1", "boot-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch on: %v, want %v", got, want)
	}
	r = newReplayerWithMappingDB(&fakeMappingDB{startup: mapping.Startup})
	if got, want := r.backgroundMockNames(context.Background(), "s"), []string{"boot-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a startup-only store: %v, want %v", got, want)
	}
	r = newReplayerWithMappingDB(&shMappingDB{prMappingDB: &prMappingDB{}})
	r.exact = &exactPlan{worker: []models.MockEntry{{Name: "poll-1"}}}
	if got, want := r.backgroundMockNames(context.Background(), "s"), []string{"poll-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch on, no file: %v, want the served worker scope %v", got, want)
	}
	for _, db := range []MappingDB{&fakeMappingDB{startupErr: errors.New("unreadable")}, unreadableMappingDB{&prMappingDB{}}} {
		r = newReplayerWithMappingDB(db)
		r.exact = &exactPlan{worker: []models.MockEntry{{Name: "poll-1"}}}
		if got := r.backgroundMockNames(context.Background(), "s"); got != nil {
			t.Fatalf("an unreadable file (%T) named %v", db, got)
		}
	}
	if got := (&Replayer{logger: zap.NewNop()}).backgroundMockNames(context.Background(), "s"); got != nil {
		t.Fatalf("no mapping store named %v", got)
	}
}

// unreadableMappingDB is a mapping store whose mappings.yaml cannot be read.
type unreadableMappingDB struct{ *prMappingDB }

func (unreadableMappingDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return nil, errors.New("unreadable")
}

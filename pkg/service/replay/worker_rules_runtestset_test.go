package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// keploy test hands the agent the mocks a set's worker-mock rules marked (worker-scope
// entries keploy record marked models.WorkerRuleReason("jwks")) in the session
// pool, so whichever test reaches a cold process is answered by them; the
// test's other mocks stay in the per-test pool, and a worker entry without
// the mark is left where its test put it.
func TestRunTestSetStoresTheWorkerMocksAsSessionMocks(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		name := "the timestamp path"
		if mapped {
			name = "the mapping path"
		}
		t.Run(name, func(t *testing.T) { runStoresTheWorkerMocksAsSessionMocks(t, mapped) })
	}
}

func runStoresTheWorkerMocksAsSessionMocks(t *testing.T, mapped bool) {
	t.Setenv(provenswitch.Env, "")
	h := newPartialRunHarness(t, 2, 0)
	if mapped {
		h.mappings.onDisk = map[string][]models.MockEntry{"test-1": {{Name: "jwks-1"}, {Name: "own-1"}, {Name: "poll-1"}}}
	}
	perTest := func(name string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://idp/" + name}, HTTPResp: &models.HTTPResp{StatusCode: 200}},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	h.mocks.filtered = []*models.Mock{perTest("jwks-1"), perTest("own-1"), perTest("poll-1")}
	h.replayer.mappingDB = &shMappingDB{prMappingDB: h.mappings, mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "jwks-1"}, {Name: "own-1"}, {Name: "poll-1"}}}},
		Worker:    []models.MockEntry{{Name: "jwks-1", Reason: models.WorkerRuleReason("jwks")}, {Name: "poll-1"}},
	}}
	h.run(t)

	h.instr.mu.Lock()
	filtered, unfiltered := h.instr.storedFiltered, h.instr.storedUnfiltered
	h.instr.mu.Unlock()
	var session *models.Mock
	for _, m := range unfiltered {
		if m.Name == "jwks-1" {
			session = m
		}
	}
	if session == nil || session.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatalf("session pool %v: want jwks-1 in it, of session lifetime", names(unfiltered))
	}
	if got := names(filtered); len(got) != 2 || got[0] != "own-1" || got[1] != "poll-1" {
		t.Fatalf("per-test pool %v; want [own-1 poll-1]", got)
	}
}

// A run that prunes unused mocks keeps the mocks a set's worker-mock rules marked, consumed
// or not: a run answers every cold process with the copy it reaches first, so
// the others go unconsumed, and the next run's cold processes need them.
func TestRunTestSetPruneKeepsTheWorkerMocks(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Test.RemoveUnusedMocks = true
	h.replayer.mappingDB = &shMappingDB{prMappingDB: h.mappings, mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "jwks-1"}, {Name: "own-1"}}}},
		Worker:    []models.MockEntry{{Name: "jwks-1", Reason: models.WorkerRuleReason("jwks")}, {Name: "jwks-2", Reason: models.WorkerRuleReason("jwks")}},
	}}
	h.run(t)

	h.mocks.mu.Lock()
	kept, calls := h.mocks.kept, h.mocks.updateCalls
	h.mocks.mu.Unlock()
	if calls != 1 {
		t.Fatalf("made %d prunes; want 1", calls)
	}
	for _, n := range []string{"jwks-1", "jwks-2"} {
		if _, ok := kept[n]; !ok {
			t.Fatalf("the prune's keep-set %v leaves out the worker mock %s", kept, n)
		}
	}
}

func names(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

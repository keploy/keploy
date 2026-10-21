package replay

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// concurrent_shares_runtestset_test.go: the shares through OSS's own replay
// loop (RunTestSet). Two tests were recorded while both requests were open;
// both calls are listed under test-1, the test whose request came first, in
// one concurrency group. test-2 made c2.

// shareInstr is the stand-in agent of a replay under exact serving whose app
// makes one call per test case (calls): the agent answers it with a recording
// the test owns, or with one the replay shares with it
// (models.ExactServing.Concurrent) that no other test answered with; any other
// call is a miss. serves is what it says on /storemocks: as the keploy agent
// client, nothing until a store has answered (stored).
type shareInstr struct {
	*prInstr
	serves  bool
	stored  bool
	calls   map[string]string
	claimed map[string]string
	pending []models.MockState
	misses  []models.UnmatchedCall
	// shared counts the opens that named shared recordings.
	shared int
}

func (s *shareInstr) ServesConcurrent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serves && s.stored
}

func (s *shareInstr) StoreMocks(ctx context.Context, filtered, unfiltered []*models.Mock) error {
	err := s.prInstr.StoreMocks(ctx, filtered, unfiltered)
	s.mu.Lock()
	s.stored = true
	s.mu.Unlock()
	return err
}

func (s *shareInstr) UpdateMockParams(ctx context.Context, p models.MockFilterParams) error {
	if ex := p.Exact; ex != nil && ex.Test != "" {
		s.mu.Lock()
		call := s.calls[ex.Test]
		s.pending, s.misses = nil, nil
		if len(ex.Concurrent) > 0 {
			s.shared++
		}
		state := models.MockState{Name: call, Kind: shareKind, Usage: models.Deleted, IsFiltered: true, Lifetime: models.LifetimePerTest}
		switch claimant := s.claimed[call]; {
		case call == "":
			// The test makes no call.
		case slices.Contains(ex.Owned, call):
			s.pending = []models.MockState{state}
		case slices.Contains(ex.Concurrent, call) && (claimant == "" || claimant == ex.Test):
			s.claimed[call] = ex.Test
			state.Concurrent = true
			s.pending = []models.MockState{state}
		default:
			s.misses = []models.UnmatchedCall{{Protocol: "ShareTest", ActualSummary: "INSERT", Kind: models.MissInTest, MatchPhase: "no_schema_candidates"}}
		}
		s.mu.Unlock()
	}
	return s.prInstr.UpdateMockParams(ctx, p)
}

func (s *shareInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = nil
	return out, nil
}

// shMappingDB is prMappingDB that reads the whole mapping, its concurrency
// groups included (mappingReader).
type shMappingDB struct {
	*prMappingDB
	mapping *models.Mapping
}

func (m *shMappingDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return m.mapping, nil
}

// shMockDB is prMockDB that keeps what the learn step learned (here,
// nothing): the full round writes it.
type shMockDB struct{ *prMockDB }

func (shMockDB) PersistLearned(context.Context, string, map[string]models.MockState) (map[string][]string, error) {
	return nil, nil
}

// seenHooks are the replay's hooks, keeping what BeforeTestResult was handed.
type seenHooks struct {
	TestHooks
	seen []models.TestResult
}

func (h *seenHooks) BeforeTestResult(ctx context.Context, testRunID, testSetID string, results []models.TestResult) error {
	for _, r := range results {
		r.FailureInfo.UnconsumedMocks = slices.Clone(r.FailureInfo.UnconsumedMocks)
		h.seen = append(h.seen, r)
	}
	return h.TestHooks.BeforeTestResult(ctx, testRunID, testSetID, results)
}

// shareRun replays the set under test's verdict settings with an agent that
// serves shared recordings when serves, and returns the harness and the agent.
// calls are the calls the app makes, by test: test-1 c1 and test-2 c2 when
// nil.
func shareRun(t *testing.T, test config.Test, serves bool, calls ...map[string]string) (*tmplHarness, *shareInstr) {
	t.Helper()
	registerShareKeys(t)
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		for _, p := range []string{"/companies/one", "/companies/two"} {
			mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				writeJSON(w, http.StatusOK, "made")
			})
		}
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "POST", "/companies/one", nil, "", recorded(200, nil, "made", nil)),
		tmplCase(2, app.addr, "POST", "/companies/two", nil, "", recorded(200, nil, "made", nil)),
	}
	at := cases[0].HTTPReq.Timestamp
	c1 := shMock("c1", 0, "INSERT", "bind.$1=one", "id=1")
	c2 := shMock("c2", 0, "INSERT", "bind.$1=two", "id=2")
	c1.Spec.ReqTimestampMock, c2.Spec.ReqTimestampMock = at.Add(1200e6), at.Add(1300e6)
	c1.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}
	c2.TestModeInfo = c1.TestModeInfo
	h := newTmplHarness(t, &tmplConf{}, cases, func(c *config.Config) {
		c.Test.Learn, c.Test.ShareConcurrent = test.Learn, test.ShareConcurrent
	})
	h.r.mockDB = shMockDB{&prMockDB{filtered: []*models.Mock{c1, c2}}}
	h.r.mappingDB = &shMappingDB{prMappingDB: &prMappingDB{}, mapping: &models.Mapping{
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{shEntry(c1, "g1"), shEntry(c2, "g1")}}, {ID: "test-2", Mocks: []models.MockEntry{}}},
		Groups:    []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"test-1", "test-2"}}},
	}}
	instr := &shareInstr{prInstr: h.r.instrumentation.(*prInstr), serves: serves,
		calls: map[string]string{"test-1": "c1", "test-2": "c2"}, claimed: map[string]string{}}
	if len(calls) > 0 {
		instr.calls = calls[0]
	}
	instr.cases = cases
	instr.mockErrorsFor = func() ([]models.UnmatchedCall, error) {
		instr.mu.Lock()
		defer instr.mu.Unlock()
		out := instr.misses
		instr.misses = nil
		return out, nil
	}
	h.r.instrumentation = instr
	h.r.hookImpl = &seenHooks{TestHooks: newHooks(prLogger(), h.r.config, instr, false)}
	h.run(t)
	return h, instr
}

// keploy record's full round (Learn and ShareConcurrent) with an agent that
// serves shared recordings: each test answers its own call, test-2 with the
// recording listed under test-1, and the set's end pins each to the test that
// made it.
func TestRunTestSet_TheFullRoundSharesTheRecordingsOfTestsRecordedTogether(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h, instr := shareRun(t, config.Test{Learn: true, ShareConcurrent: true}, true)
	assertStatus(t, h, "test-1", models.TestStatusPassed, "it made its own call, and is not required to make test-2's")
	assertStatus(t, h, "test-2", models.TestStatusPassed, "its call's recording, listed under test-1, was shared with it")
	if instr.shared == 0 {
		t.Fatal("the full round shared no recording")
	}
	got, ok := h.r.exact.setShares(models.TestSetStatusPassed)
	if !ok || got.State != models.SharesComplete || got.Pins["c1"] != "test-1" || got.Pins["c2"] != "test-2" {
		t.Fatalf("shares %+v (%v); want c1 pinned to test-1 and c2 to test-2", got, ok)
	}
}

// The sandbox's and keploy mock record's rounds run the learn step (Learn)
// and share nothing, with an agent that serves shared recordings too: no step
// after them pins what a shared round settles, so their verdicts are strict
// keploy test's, test for test, which CI gives too.
func TestRunTestSet_ALearnRoundThatPinsNothingSharesNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	strict, _ := shareRun(t, config.Test{}, true)
	learnOnly, instr := shareRun(t, config.Test{Learn: true}, true)
	if instr.shared != 0 {
		t.Fatalf("a learn round that pins nothing named shared recordings %d times", instr.shared)
	}
	for _, test := range []string{"test-1", "test-2"} {
		s, l := strict.result(t, test), learnOnly.result(t, test)
		if s.Status != l.Status {
			t.Fatalf("%s: %s in the learn round, %s in strict keploy test", test, l.Status, s.Status)
		}
	}
	// The fixture is one strict keploy test fails as recorded: test-1 is
	// required to make test-2's call, and test-2 has no recording of its own.
	assertStatus(t, strict, "test-1", models.TestStatusFailed, "it lists test-2's call")
	assertStatus(t, strict, "test-2", models.TestStatusFailed, "it lists no recording of its call")
	if _, sharing := learnOnly.r.exact.setShares(models.TestSetStatusFailed); sharing {
		t.Fatal("a learn round that pins nothing has shares for its set's end")
	}
}

// What the set's end settles charges the tests before any hook reads the
// results: a before-test-result hook (the auto-noise hook learns noise on a
// failed test it is handed, and may pass it) is handed a charged test FAILED,
// with the evidence. Here test-2 makes no call: c2, shared between both tests,
// was made by neither, and each is charged with it.
func TestRunTestSet_TheSettlementChargesBeforeAnyHookReadsTheResults(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h, _ := shareRun(t, config.Test{Learn: true, ShareConcurrent: true}, true, map[string]string{"test-1": "c1"})
	seen := h.r.hookImpl.(*seenHooks).seen
	if len(seen) == 0 {
		t.Fatal("no hook was handed the results")
	}
	for _, r := range seen {
		var charged bool
		for _, u := range r.FailureInfo.UnconsumedMocks {
			charged = charged || (u.Name == "c2" && u.Concurrent != "")
		}
		if r.Status != models.TestStatusFailed || !charged {
			t.Fatalf("%s was handed to the hook %s with unconsumed %+v; want FAILED, charged with c2", r.TestCaseID, r.Status, r.FailureInfo.UnconsumedMocks)
		}
	}
}

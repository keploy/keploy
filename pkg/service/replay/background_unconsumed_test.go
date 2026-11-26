package replay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type bgUnfilteredMockDB struct {
	*prMockDB
	unfiltered []*models.Mock
}

func (m *bgUnfilteredMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return m.unfiltered, nil
}

type bgStreamConsumingHooks struct {
	TestHooks
	mu       sync.Mutex
	sent     bool
	consumed []models.MockState
}

func (h *bgStreamConsumingHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	h.mu.Lock()
	h.sent = true
	h.mu.Unlock()
	return h.TestHooks.SimulateRequest(ctx, tc, testSetID)
}

func (h *bgStreamConsumingHooks) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sent {
		return nil, nil
	}
	out := h.consumed
	h.consumed = nil
	return out, nil
}

type bgConsumedErrHooks struct {
	prHooks
	err error
}

func (h bgConsumedErrHooks) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, h.err
}

func perTestMongoMock(name string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":                     "read",
				models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
			},
			ReqTimestampMock: attrAt(1010),
			ResTimestampMock: attrAt(1015),
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        models.LifetimePerTest,
			LifetimeDerived: true,
			IsFiltered:      true,
		},
	}
}

// A test case whose only unconsumed recorded mocks are background, worker-scoped,
// or startup mocks must not be marked OBSOLETE (nor failed as missing a per-test
// dependency) when unmatched calls are 0 with KEPLOY_PROVEN unset. Under
// KEPLOY_PROVEN=1 (exact serving), post-plan exact ownership is the single source
// of truth: tests: owns its mocks over worker:/startup: without split-brain,
// worker-rule entries dropped by WithoutWorkerRuleEntries stay enforced, rejected
// source: keploy-test startup entries cannot excuse window-owned mocks, and
// required worker mocks (non-s12 reached or Trails != "") stay enforced at set
// end by unansweredWorker.
func TestRunTestSet_UnconsumedBackgroundOrWorkerMocks(t *testing.T) {
	own1 := func() *models.Mock { return perTestMongoMock("mock-own-1") }
	own2 := func() *models.Mock { return perTestMongoMock("mock-own-2") }
	bg1 := func() *models.Mock { return perTestMongoMock("mock-bg-1") }
	trail1 := func() *models.Mock { return perTestMongoMock("mock-trail-1") }
	boot1 := func() *models.Mock { return perTestMongoMock("mock-boot-1") }
	jwksFiltered := func() *models.Mock { return perTestMongoMock("mock-jwks-1") }
	jwksUnfiltered := func() *models.Mock {
		m := perTestMongoMock("mock-jwks-1")
		m.TestModeInfo.IsFiltered = false
		return m
	}
	drift1 := func() *models.Mock { return perTestMongoMock("mock-drift-1") }

	own1Consumed := []models.MockState{{
		Name:     "mock-own-1",
		Kind:     models.Mongo,
		Type:     "read",
		Lifetime: models.LifetimePerTest,
	}}

	cases := []struct {
		name              string
		proven            bool
		s12Enabled        bool
		recordedWith      []string
		mappingSource     string
		pinTestWindows    bool
		assertDeps        bool
		strictFailure     bool
		wrongBody         bool
		unmatched         []models.UnmatchedCall
		filtered          func() []*models.Mock
		unfiltered        func() []*models.Mock
		testEntries       []models.MockEntry
		workerEntries     []models.MockEntry
		startupEntries    []models.MockEntry
		wantSetStatus     models.TestSetStatus
		wantTestStatus    models.TestStatus
		wantMissingRows   int
		wantUnconsumed    []models.UnconsumedMock
		wantObsoleteInfo  bool
		wantNoUnexercised bool
		wantOwned         []string
		wantInWorker      []string
		wantNotInWorker   []string
	}{
		{
			name:          "switch off auto-replay response diff with only worker bg mock unconsumed is FAILED not OBSOLETE",
			proven:        false,
			strictFailure: false,
			wrongBody:     true,
			filtered:      func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-bg-1", Kind: string(models.Mongo)},
			},
			workerEntries:   []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 0,
		},
		{
			name:          "switch off assert-dependencies response matches with only worker bg mock unconsumed passes",
			proven:        false,
			assertDeps:    true,
			strictFailure: false,
			filtered:      func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-bg-1", Kind: string(models.Mongo)},
			},
			workerEntries:     []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
			wantSetStatus:     models.TestSetStatusPassed,
			wantTestStatus:    models.TestStatusPassed,
			wantMissingRows:   0,
			wantNoUnexercised: true,
		},
		{
			name:          "switch off default auto-replay response matches with only worker bg mock unconsumed has no missing rows or warn",
			proven:        false,
			strictFailure: false,
			filtered:      func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-bg-1", Kind: string(models.Mongo)},
			},
			workerEntries:     []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
			wantSetStatus:     models.TestSetStatusPassed,
			wantTestStatus:    models.TestStatusPassed,
			wantMissingRows:   0,
			wantNoUnexercised: true,
		},
		{
			name:          "switch off cloud-replay strictFailure response matches with only worker bg mock unconsumed matches auto-replay",
			proven:        false,
			strictFailure: true,
			filtered:      func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-bg-1", Kind: string(models.Mongo)},
			},
			workerEntries:     []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
			wantSetStatus:     models.TestSetStatusPassed,
			wantTestStatus:    models.TestStatusPassed,
			wantMissingRows:   0,
			wantNoUnexercised: true,
		},
		{
			name:          "switch off auto-replay response diff with only startup per-test mock unconsumed is FAILED not OBSOLETE",
			proven:        false,
			strictFailure: false,
			wrongBody:     true,
			filtered:      func() []*models.Mock { return []*models.Mock{own1(), boot1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-boot-1", Kind: string(models.Mongo)},
			},
			startupEntries:  []models.MockEntry{{Name: "mock-boot-1", Kind: string(models.Mongo)}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 0,
		},
		{
			name:       "switch off assert-dependencies response matches with only startup per-test mock unconsumed passes",
			proven:     false,
			assertDeps: true,
			filtered:   func() []*models.Mock { return []*models.Mock{own1(), boot1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-boot-1", Kind: string(models.Mongo)},
			},
			startupEntries:  []models.MockEntry{{Name: "mock-boot-1", Kind: string(models.Mongo)}},
			wantSetStatus:   models.TestSetStatusPassed,
			wantTestStatus:  models.TestStatusPassed,
			wantMissingRows: 0,
		},
		{
			name:          "switch off auto-replay response diff with worker-rule mock in unfiltered pool is FAILED not OBSOLETE",
			proven:        false,
			strictFailure: false,
			wrongBody:     true,
			filtered:      func() []*models.Mock { return []*models.Mock{own1()} },
			unfiltered:    func() []*models.Mock { return []*models.Mock{jwksUnfiltered()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-jwks-1", Kind: string(models.Mongo)},
			},
			workerEntries:   []models.MockEntry{{Name: "mock-jwks-1", Kind: string(models.Mongo), Reason: models.WorkerRuleReason("jwks")}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 0,
		},
		{
			name:         "switch on s12_routing worker-only non-trailing bg mock unconsumed passes",
			proven:       true,
			s12Enabled:   true,
			recordedWith: []string{models.RecordedWithS12Routing},
			filtered:     func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
			},
			workerEntries:   []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo), Place: "test-1"}},
			wantSetStatus:   models.TestSetStatusPassed,
			wantTestStatus:  models.TestStatusPassed,
			wantMissingRows: 0,
			wantOwned:       []string{"mock-own-1"},
			wantInWorker:    []string{"mock-bg-1"},
		},
		{
			name:     "switch on startup-only mock in pool unconsumed passes",
			proven:   true,
			filtered: func() []*models.Mock { return []*models.Mock{own1(), boot1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
			},
			startupEntries:  []models.MockEntry{{Name: "mock-boot-1", Kind: string(models.Mongo)}},
			wantSetStatus:   models.TestSetStatusPassed,
			wantTestStatus:  models.TestStatusPassed,
			wantMissingRows: 0,
			wantOwned:       []string{"mock-own-1"},
		},
		{
			name:     "negative: switch on dual-listed tests and worker mock stays owned and enforced by test without split-brain",
			proven:   true,
			filtered: func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-bg-1", Kind: string(models.Mongo)},
			},
			workerEntries:   []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo), Place: "test-1"}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
			wantUnconsumed:  []models.UnconsumedMock{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
			wantOwned:       []string{"mock-own-1", "mock-bg-1"},
			wantNotInWorker: []string{"mock-bg-1"},
		},
		{
			name:     "negative: switch on non-s12 reached worker-only mock stays enforced at set end by unansweredWorker",
			proven:   true,
			filtered: func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
			},
			workerEntries:   []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo), Place: "test-1"}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusPassed,
			wantMissingRows: 0,
			wantOwned:       []string{"mock-own-1"},
			wantInWorker:    []string{"mock-bg-1"},
		},
		{
			name:         "negative: switch on trailing write worker mock stays enforced at set end even under s12_routing",
			proven:       true,
			s12Enabled:   true,
			recordedWith: []string{models.RecordedWithS12Routing},
			filtered:     func() []*models.Mock { return []*models.Mock{own1(), trail1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
			},
			workerEntries:   []models.MockEntry{{Name: "mock-trail-1", Kind: string(models.Mongo), Place: "test-1", Trails: "test-1"}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusPassed,
			wantMissingRows: 0,
			wantOwned:       []string{"mock-own-1"},
			wantInWorker:    []string{"mock-trail-1"},
		},
		{
			name:     "negative: switch on dual-listed tests and undeclared startup mock stays owned and enforced by test",
			proven:   true,
			filtered: func() []*models.Mock { return []*models.Mock{own1(), boot1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-boot-1", Kind: string(models.Mongo)},
			},
			startupEntries:  []models.MockEntry{{Name: "mock-boot-1", Kind: string(models.Mongo)}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
			wantUnconsumed:  []models.UnconsumedMock{{Name: "mock-boot-1", Kind: string(models.Mongo)}},
			wantOwned:       []string{"mock-own-1", "mock-boot-1"},
		},
		{
			name:     "negative: switch on worker-rule mock in test mapping stays enforced when dropped by WithoutWorkerRuleEntries",
			proven:   true,
			filtered: func() []*models.Mock { return []*models.Mock{own1(), jwksFiltered()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-jwks-1", Kind: string(models.Mongo)},
			},
			workerEntries:   []models.MockEntry{{Name: "mock-jwks-1", Kind: string(models.Mongo), Reason: models.WorkerRuleReason("jwks")}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
			wantUnconsumed:  []models.UnconsumedMock{{Name: "mock-jwks-1", Kind: string(models.Mongo)}},
			wantOwned:       []string{"mock-own-1", "mock-jwks-1"},
			wantNotInWorker: []string{"mock-jwks-1"},
		},
		{
			name:           "negative: switch on source keploy-test does not excuse window-owned mock via rejected startup",
			proven:         true,
			mappingSource:  models.MappingSourceKeployTest,
			pinTestWindows: true,
			filtered:       func() []*models.Mock { return []*models.Mock{own1(), own2()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
			},
			startupEntries:  []models.MockEntry{{Name: "mock-own-2", Kind: string(models.Mongo)}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
			wantUnconsumed:  []models.UnconsumedMock{{Name: "mock-own-2", Kind: string(models.Mongo)}},
			wantOwned:       []string{"mock-own-1", "mock-own-2"},
		},
		{
			name:          "negative: switch off auto-replay response diff with genuine per-test mock unconsumed stays OBSOLETE",
			proven:        false,
			strictFailure: false,
			wrongBody:     true,
			filtered:      func() []*models.Mock { return []*models.Mock{own1(), own2()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-own-2", Kind: string(models.Mongo)},
			},
			wantSetStatus:    models.TestSetStatusInternalErr,
			wantTestStatus:   models.TestStatusObsolete,
			wantMissingRows:  1,
			wantObsoleteInfo: true,
		},
		{
			name:       "negative: switch off assert-dependencies with genuine per-test mock unconsumed still fails",
			proven:     false,
			assertDeps: true,
			filtered:   func() []*models.Mock { return []*models.Mock{own1(), own2()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-own-2", Kind: string(models.Mongo)},
			},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
		},
		{
			name:     "negative: switch on with genuine per-test mock unconsumed still fails",
			proven:   true,
			filtered: func() []*models.Mock { return []*models.Mock{own1(), own2()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-own-2", Kind: string(models.Mongo)},
			},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
			wantUnconsumed:  []models.UnconsumedMock{{Name: "mock-own-2", Kind: string(models.Mongo)}},
		},
		{
			name:     "negative: switch on returned drift move in worker still fails owner when unconsumed",
			proven:   true,
			filtered: func() []*models.Mock { return []*models.Mock{own1(), drift1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
			},
			workerEntries: []models.MockEntry{
				{Name: "mock-drift-1", Kind: string(models.Mongo), Owner: "test-1", Drift: true, ReqTimestampMock: models.FormatMockTimestamp(attrAt(1020))},
			},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
			wantUnconsumed:  []models.UnconsumedMock{{Name: "mock-drift-1", Kind: string(models.Mongo)}},
		},
		{
			name:         "negative: switch on unmatched data-plane call still fails even when worker bg mock is in worker scope",
			proven:       true,
			s12Enabled:   true,
			recordedWith: []string{models.RecordedWithS12Routing},
			filtered:     func() []*models.Mock { return []*models.Mock{own1(), bg1()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
			},
			workerEntries: []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo), Place: "test-1"}},
			unmatched: []models.UnmatchedCall{{
				Protocol:      "MongoDB",
				ActualSummary: "find users",
				MatchPhase:    models.MatchPhaseBody,
			}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 0,
		},
		{
			name:          "negative: switch off unmatched data-plane call vetoes OBSOLETE when genuine mock is unconsumed",
			proven:        false,
			strictFailure: false,
			wrongBody:     true,
			filtered:      func() []*models.Mock { return []*models.Mock{own1(), own2()} },
			testEntries: []models.MockEntry{
				{Name: "mock-own-1", Kind: string(models.Mongo)},
				{Name: "mock-own-2", Kind: string(models.Mongo)},
			},
			unmatched: []models.UnmatchedCall{{
				Protocol:      "MongoDB",
				ActualSummary: "find users",
				MatchPhase:    models.MatchPhaseBody,
			}},
			wantSetStatus:   models.TestSetStatusFailed,
			wantTestStatus:  models.TestStatusFailed,
			wantMissingRows: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, tc.proven)
			if tc.proven {
				fastWorkerWait(t)
			}
			if tc.s12Enabled {
				on := true
				enableS12(t, &on)
			}
			h := newPartialRunHarness(t, 1, 0)
			if tc.pinTestWindows {
				pinWindows(h)
			}
			core, logs := observer.New(zap.DebugLevel)
			h.replayer.logger = zap.New(core)
			h.replayer.config.Test.AssertDependencies = tc.assertDeps
			h.replayer.config.Test.StrictFailure = tc.strictFailure
			h.mocks.filtered = tc.filtered()
			if tc.unfiltered != nil {
				h.replayer.mockDB = &bgUnfilteredMockDB{prMockDB: h.mocks, unfiltered: tc.unfiltered()}
			}
			h.mappings.onDisk = map[string][]models.MockEntry{"test-1": tc.testEntries}
			h.mappings.onDiskStartup = tc.startupEntries
			h.replayer.mappingDB = &shMappingDB{
				prMappingDB: h.mappings,
				mapping: &models.Mapping{
					Source:       tc.mappingSource,
					RecordedWith: tc.recordedWith,
					TestCases:    []models.MappedTestCase{{ID: "test-1", Mocks: tc.testEntries}},
					Worker:       tc.workerEntries,
					Startup:      tc.startupEntries,
				},
			}
			hooks := prHooks{consumed: own1Consumed}
			if tc.wrongBody {
				hooks.wrongBody = map[string]bool{"test-1": true}
			}
			h.replayer.hookImpl = hooks
			if len(tc.unmatched) > 0 {
				sent := false
				h.instr.mockErrorsFor = func() ([]models.UnmatchedCall, error) {
					if sent {
						return nil, nil
					}
					sent = true
					return tc.unmatched, nil
				}
			}

			setStatus := h.run(t)
			got := resultOf(t, h, "test-1")
			if setStatus != tc.wantSetStatus || got.Status != tc.wantTestStatus {
				t.Fatalf("set=%s test=%s (depsChecked=%v depsConsumed=%d depResult=%+v unconsumed=%+v mappingStale=%q), want set=%s test=%s",
					setStatus, got.Status, got.Result.DepsChecked, got.Result.DepsConsumed,
					got.Result.DepResult, got.FailureInfo.UnconsumedMocks, got.FailureInfo.MappingStale,
					tc.wantSetStatus, tc.wantTestStatus)
			}
			if len(got.Result.DepResult) != tc.wantMissingRows {
				t.Fatalf("DepResult count = %d (%+v), want %d", len(got.Result.DepResult), got.Result.DepResult, tc.wantMissingRows)
			}
			if !got.Result.DepsChecked || got.Result.DepsConsumed != 1 {
				t.Fatalf("DepsChecked=%v DepsConsumed=%d, want true and 1", got.Result.DepsChecked, got.Result.DepsConsumed)
			}
			if len(tc.wantUnconsumed) > 0 {
				if !reflect.DeepEqual(got.FailureInfo.UnconsumedMocks, tc.wantUnconsumed) {
					t.Fatalf("UnconsumedMocks = %+v, want %+v", got.FailureInfo.UnconsumedMocks, tc.wantUnconsumed)
				}
			} else if len(got.FailureInfo.UnconsumedMocks) != 0 {
				t.Fatalf("expected no UnconsumedMocks, got %+v", got.FailureInfo.UnconsumedMocks)
			}
			if tc.wantObsoleteInfo {
				if got.FailureInfo.MappingStale == "" || got.FailureInfo.MockMismatch == nil {
					t.Fatalf("expected OBSOLETE mapping stale + mock mismatch info, got stale=%q mismatch=%+v",
						got.FailureInfo.MappingStale, got.FailureInfo.MockMismatch)
				}
			} else {
				if got.FailureInfo.MappingStale != "" || got.FailureInfo.MockMismatch != nil {
					t.Fatalf("unexpected OBSOLETE info on non-OBSOLETE test: stale=%q mismatch=%+v",
						got.FailureInfo.MappingStale, got.FailureInfo.MockMismatch)
				}
			}
			if tc.wantNoUnexercised {
				warns := logs.FilterMessageSnippet("some tests did not exercise a dependency").All()
				if len(warns) != 0 {
					t.Fatalf("unexpected unexercised-dependency warning: %+v", warns)
				}
			}
			if len(tc.wantOwned) > 0 {
				if h.replayer.exact == nil || !reflect.DeepEqual(h.replayer.exact.owned["test-1"], tc.wantOwned) {
					t.Fatalf("exact.owned[test-1] = %v, want %v", h.replayer.exact.owned["test-1"], tc.wantOwned)
				}
			}
			for _, name := range tc.wantInWorker {
				if h.replayer.exact == nil || !h.replayer.exact.isWorker(name) {
					t.Fatalf("expected %q in exact.worker, got %+v", name, h.replayer.exact.worker)
				}
			}
			for _, name := range tc.wantNotInWorker {
				if h.replayer.exact != nil && h.replayer.exact.isWorker(name) {
					t.Fatalf("expected %q NOT in exact.worker, got %+v", name, h.replayer.exact.worker)
				}
			}
		})
	}
}

// Under KEPLOY_PROVEN=1, ownsTestMocks and testMocksUnknown must agree on a
// dual-listed tests: + worker: mock: when that mock is the test's sole mapped
// dependency and GetConsumedMocks fails, the set must not silently pass.
func TestRunTestSet_ProvenDualListedTestMocksUnknownAgreesWithOwnsTestMocks(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 1, 0)
	h.mocks.filtered = []*models.Mock{perTestMongoMock("mock-bg-1")}
	entries := []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}}
	h.mappings.onDisk = map[string][]models.MockEntry{"test-1": entries}
	h.replayer.mappingDB = &shMappingDB{
		prMappingDB: h.mappings,
		mapping: &models.Mapping{
			TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: entries}},
			Worker:    []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
		},
	}
	h.replayer.hookImpl = bgConsumedErrHooks{err: errors.New("agent consumed read failed")}

	setStatus := h.run(t)
	if h.replayer.exact == nil || !h.replayer.exact.ownsTestMocks {
		t.Fatalf("expected exact.ownsTestMocks=true, got %+v", h.replayer.exact)
	}
	if setStatus == models.TestSetStatusPassed {
		t.Fatalf("set passed despite GetConsumedMocks error on test-owned dual-listed mock; ownsTestMocks=%v", h.replayer.exact.ownsTestMocks)
	}
}

// Streaming pass: consuming a worker-scoped or pool startup mock during a
// failed streaming test must not trigger a mock-set mismatch (neither demoting
// to OBSOLETE with KEPLOY_PROVEN unset nor recording a spurious mismatch under
// KEPLOY_PROVEN=1), while a dual-listed tests: + worker: mock left unconsumed
// under KEPLOY_PROVEN=1 stays owned and enforced by the streaming test.
func TestRunTestSet_StreamedUnconsumedOrConsumedBackgroundMock(t *testing.T) {
	const expected = `{"orders":[{"id":"ord_LIT","status":"paid"}]}`

	t.Run("switch on: consuming worker-scope mock during failed streaming test records no mock mismatch", func(t *testing.T) {
		setSwitch(t, true)
		fastWorkerWait(t)
		on := true
		enableS12(t, &on)
		app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
			mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"orders":[{"id":"ord_DIFF","status":"paid"}]}`+"\n")
			})
		})
		cases := []*models.TestCase{
			tmplCase(1, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       expected,
				StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: expected}}}},
			}),
		}
		ownMock := perTestMongoMock("mock-own-stream")
		bgMock := perTestMongoMock("mock-bg-stream")
		h := newTmplHarness(t, &tmplConf{}, cases, nil)
		h.r.mockDB = &prMockDB{filtered: []*models.Mock{ownMock, bgMock}}
		h.r.mappingDB = &shMappingDB{
			prMappingDB: &prMappingDB{
				onDisk: map[string][]models.MockEntry{"test-1": {{Name: "mock-own-stream", Kind: string(models.Mongo)}}},
			},
			mapping: &models.Mapping{
				RecordedWith: []string{models.RecordedWithS12Routing},
				TestCases:    []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-own-stream", Kind: string(models.Mongo)}}}},
				Worker:       []models.MockEntry{{Name: "mock-bg-stream", Kind: string(models.Mongo)}},
			},
		}
		h.r.hookImpl = &bgStreamConsumingHooks{
			TestHooks: h.r.hookImpl,
			consumed: []models.MockState{
				{Name: "mock-own-stream", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
				{Name: "mock-bg-stream", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
			},
		}
		core, _ := observer.New(zap.DebugLevel)
		h.r.logger = zap.New(core)

		st := h.run(t)
		got := h.result(t, "test-1")
		if st != models.TestSetStatusFailed || got.Status != models.TestStatusFailed {
			t.Fatalf("set=%s test=%s; want FAILED", st, got.Status)
		}
		if failures := h.r.mockMismatchFailures.GetFailures(); len(failures) != 0 {
			t.Fatalf("unexpected mockMismatchFailures for worker-scope mock during streaming test: %+v", failures)
		}
	})

	t.Run("switch on: consuming pool startup mock during failed streaming test records no mock mismatch", func(t *testing.T) {
		setSwitch(t, true)
		fastWorkerWait(t)
		app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
			mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"orders":[{"id":"ord_DIFF","status":"paid"}]}`+"\n")
			})
		})
		cases := []*models.TestCase{
			tmplCase(1, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       expected,
				StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: expected}}}},
			}),
		}
		ownMock := perTestMongoMock("mock-own-stream")
		bootMock := perTestMongoMock("mock-boot-stream")
		h := newTmplHarness(t, &tmplConf{}, cases, nil)
		h.r.mockDB = &prMockDB{filtered: []*models.Mock{ownMock, bootMock}}
		h.r.mappingDB = &shMappingDB{
			prMappingDB: &prMappingDB{
				onDisk:        map[string][]models.MockEntry{"test-1": {{Name: "mock-own-stream", Kind: string(models.Mongo)}}},
				onDiskStartup: []models.MockEntry{{Name: "mock-boot-stream", Kind: string(models.Mongo)}},
			},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-own-stream", Kind: string(models.Mongo)}}}},
				Startup:   []models.MockEntry{{Name: "mock-boot-stream", Kind: string(models.Mongo)}},
			},
		}
		h.r.hookImpl = &bgStreamConsumingHooks{
			TestHooks: h.r.hookImpl,
			consumed: []models.MockState{
				{Name: "mock-own-stream", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
				{Name: "mock-boot-stream", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
			},
		}
		core, _ := observer.New(zap.DebugLevel)
		h.r.logger = zap.New(core)

		st := h.run(t)
		got := h.result(t, "test-1")
		if st != models.TestSetStatusFailed || got.Status != models.TestStatusFailed {
			t.Fatalf("set=%s test=%s; want FAILED", st, got.Status)
		}
		if failures := h.r.mockMismatchFailures.GetFailures(); len(failures) != 0 {
			t.Fatalf("unexpected mockMismatchFailures for pool startup mock during streaming test: %+v", failures)
		}
	})

	t.Run("switch on: unconsumed dual-listed tests and worker mock in streaming test still fails", func(t *testing.T) {
		setSwitch(t, true)
		fastWorkerWait(t)
		app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
			mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, expected+"\n")
			})
		})
		cases := []*models.TestCase{
			tmplCase(1, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       expected,
				StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: expected}}}},
			}),
		}
		bgMock := perTestMongoMock("mock-bg-stream")
		h := newTmplHarness(t, &tmplConf{}, cases, nil)
		h.r.mockDB = &prMockDB{filtered: []*models.Mock{bgMock}}
		h.r.mappingDB = &shMappingDB{
			prMappingDB: &prMappingDB{
				onDisk: map[string][]models.MockEntry{"test-1": {{Name: "mock-bg-stream", Kind: string(models.Mongo)}}},
			},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-bg-stream", Kind: string(models.Mongo)}}}},
				Worker:    []models.MockEntry{{Name: "mock-bg-stream", Kind: string(models.Mongo)}},
			},
		}
		h.r.hookImpl = &consumingHooks{TestHooks: h.r.hookImpl}
		core, _ := observer.New(zap.DebugLevel)
		h.r.logger = zap.New(core)

		st := h.run(t)
		got := h.result(t, "test-1")
		if st != models.TestSetStatusFailed || got.Status != models.TestStatusFailed {
			t.Fatalf("set=%s test=%s unconsumed=%+v; want FAILED", st, got.Status, got.FailureInfo.UnconsumedMocks)
		}
		wantUnconsumed := []models.UnconsumedMock{{Name: "mock-bg-stream", Kind: string(models.Mongo)}}
		if !reflect.DeepEqual(got.FailureInfo.UnconsumedMocks, wantUnconsumed) {
			t.Fatalf("UnconsumedMocks = %+v, want %+v", got.FailureInfo.UnconsumedMocks, wantUnconsumed)
		}
	})

	t.Run("switch off: consuming extra worker bg mock during failed streaming test is FAILED not OBSOLETE", func(t *testing.T) {
		setSwitch(t, false)
		app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
			mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"orders":[{"id":"ord_DIFF","status":"paid"}]}`+"\n")
			})
		})
		cases := []*models.TestCase{
			tmplCase(1, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       expected,
				StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: expected}}}},
			}),
		}
		ownMock := perTestMongoMock("mock-own-stream")
		bgMock := perTestMongoMock("mock-bg-stream")
		h := newTmplHarness(t, &tmplConf{}, cases, nil)
		h.r.mockDB = &prMockDB{filtered: []*models.Mock{ownMock, bgMock}}
		h.r.mappingDB = &shMappingDB{
			prMappingDB: &prMappingDB{
				onDisk: map[string][]models.MockEntry{"test-1": {{Name: "mock-own-stream", Kind: string(models.Mongo)}}},
			},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-own-stream", Kind: string(models.Mongo)}}}},
				Worker:    []models.MockEntry{{Name: "mock-bg-stream", Kind: string(models.Mongo)}},
			},
		}
		h.r.hookImpl = &bgStreamConsumingHooks{
			TestHooks: h.r.hookImpl,
			consumed: []models.MockState{
				{Name: "mock-own-stream", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
				{Name: "mock-bg-stream", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
			},
		}
		core, _ := observer.New(zap.DebugLevel)
		h.r.logger = zap.New(core)

		st := h.run(t)
		got := h.result(t, "test-1")
		if st != models.TestSetStatusFailed || got.Status != models.TestStatusFailed {
			t.Fatalf("set=%s test=%s; want FAILED (not OBSOLETE)", st, got.Status)
		}
	})
}

func TestBackgroundExcludedMockNames_Direct(t *testing.T) {
	ctx := context.Background()
	db := &shMappingDB{
		prMappingDB: &prMappingDB{},
		mapping: &models.Mapping{
			Worker: []models.MockEntry{
				{Name: "poll-1"},
				{Name: "jwks-1", Reason: models.WorkerRuleReason("jwks")},
			},
			Startup: []models.MockEntry{{Name: "boot-1"}},
		},
	}

	t.Run("switch off includes startup, worker-rule, and learned worker entries when mapping enabled", func(t *testing.T) {
		setSwitch(t, false)
		r := &Replayer{logger: zap.NewNop(), instrument: true, mappingDB: db}
		got := r.backgroundExcludedMockNames(ctx, "s", true, false, []string{"boot-1", "jwks-1"})
		want := []string{"boot-1", "jwks-1", "poll-1"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("switch off with mapping disabled excludes only passed startup names", func(t *testing.T) {
		setSwitch(t, false)
		r := &Replayer{logger: zap.NewNop(), instrument: true, mappingDB: db}
		got := r.backgroundExcludedMockNames(ctx, "s", false, false, nil)
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})

	t.Run("switch on with nil exact plan returns nil", func(t *testing.T) {
		setSwitch(t, true)
		r := &Replayer{logger: zap.NewNop(), instrument: true, mappingDB: db}
		got := r.backgroundExcludedMockNames(ctx, "s", true, false, []string{"boot-1"})
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})

	t.Run("switch on fromWindows false uses post-plan exact.worker and pool startup mocks only", func(t *testing.T) {
		setSwitch(t, true)
		r := &Replayer{
			logger:     zap.NewNop(),
			instrument: true,
			mappingDB:  db,
			exact: &exactPlan{
				owned:  map[string][]string{"test-1": {"owned-startup", "dual-worker", "jwks-1"}},
				worker: []models.MockEntry{{Name: "poll-1"}},
				pool:   []string{"boot-1", "stray-pool"},
			},
		}
		got := r.backgroundExcludedMockNames(ctx, "s", true, false, []string{"boot-1", "owned-startup"})
		want := []string{"poll-1", "boot-1"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if slices.Contains(got, "stray-pool") || slices.Contains(got, "owned-startup") || slices.Contains(got, "jwks-1") {
			t.Fatalf("excluded non-background/owned mock: %v", got)
		}
	})

	t.Run("switch on fromWindows true ignores untrusted startup names even when in pool", func(t *testing.T) {
		setSwitch(t, true)
		r := &Replayer{
			logger:     zap.NewNop(),
			instrument: true,
			mappingDB:  db,
			exact: &exactPlan{
				worker: []models.MockEntry{{Name: "poll-1"}},
				pool:   []string{"boot-1"},
			},
		}
		got := r.backgroundExcludedMockNames(ctx, "s", true, true, []string{"boot-1"})
		want := []string{"poll-1"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

// When a genuine per-test mock is unconsumed alongside a background/worker mock
// (even if the background mock was consumed during the test), neither the
// persisted MockMismatchInfo nor mockMismatchFailures' actualMocks/expectedMocks
// should list the background/worker mock.
func TestRunTestSet_MockMismatchInfoExcludesBackgroundMocks(t *testing.T) {
	t.Run("switch off excludes dual-listed worker mock from MockMismatchInfo and mockMismatchFailures", func(t *testing.T) {
		setSwitch(t, false)
		h := newPartialRunHarness(t, 1, 0)
		h.mocks.filtered = []*models.Mock{
			perTestMongoMock("mock-own-1"),
			perTestMongoMock("mock-own-2"),
			perTestMongoMock("mock-bg-1"),
		}
		entries := []models.MockEntry{
			{Name: "mock-own-1", Kind: string(models.Mongo)},
			{Name: "mock-own-2", Kind: string(models.Mongo)},
			{Name: "mock-bg-1", Kind: string(models.Mongo)},
		}
		h.mappings.onDisk = map[string][]models.MockEntry{"test-1": entries}
		h.replayer.mappingDB = &shMappingDB{
			prMappingDB: h.mappings,
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: entries}},
				Worker:    []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
			},
		}
		h.replayer.hookImpl = prHooks{
			wrongBody: map[string]bool{"test-1": true},
			consumed: []models.MockState{
				{Name: "mock-own-1", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
				{Name: "mock-bg-1", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
			},
		}

		_ = h.run(t)
		got := resultOf(t, h, "test-1")
		if got.Status != models.TestStatusObsolete {
			t.Fatalf("status = %s, want OBSOLETE", got.Status)
		}
		if got.FailureInfo.MockMismatch == nil {
			t.Fatal("expected FailureInfo.MockMismatch on OBSOLETE result")
		}
		for _, m := range got.FailureInfo.MockMismatch.ExpectedMocks {
			if m.Name == "mock-bg-1" {
				t.Fatalf("FailureInfo.MockMismatch.ExpectedMocks still contains worker mock: %+v", got.FailureInfo.MockMismatch.ExpectedMocks)
			}
		}
		for _, m := range got.FailureInfo.MockMismatch.ActualMocks {
			if m.Name == "mock-bg-1" {
				t.Fatalf("FailureInfo.MockMismatch.ActualMocks still contains worker mock: %+v", got.FailureInfo.MockMismatch.ActualMocks)
			}
		}
		failures := h.replayer.mockMismatchFailures.GetFailures()
		if len(failures) != 1 {
			t.Fatalf("expected 1 mockMismatchFailure, got %d: %+v", len(failures), failures)
		}
		wantExp := []string{"mock-own-1", "mock-own-2"}
		wantAct := []string{"mock-own-1"}
		if !reflect.DeepEqual(failures[0].ExpectedMocks, wantExp) || !reflect.DeepEqual(failures[0].ActualMocks, wantAct) {
			t.Fatalf("mockMismatchFailures = exp:%v act:%v, want exp:%v act:%v",
				failures[0].ExpectedMocks, failures[0].ActualMocks, wantExp, wantAct)
		}
	})

	t.Run("switch on excludes worker-scope mock consumed during test from mockMismatchFailures", func(t *testing.T) {
		setSwitch(t, true)
		fastWorkerWait(t)
		h := newPartialRunHarness(t, 1, 0)
		h.mocks.filtered = []*models.Mock{
			perTestMongoMock("mock-own-1"),
			perTestMongoMock("mock-own-2"),
			perTestMongoMock("mock-bg-1"),
		}
		entries := []models.MockEntry{
			{Name: "mock-own-1", Kind: string(models.Mongo)},
			{Name: "mock-own-2", Kind: string(models.Mongo)},
		}
		h.mappings.onDisk = map[string][]models.MockEntry{"test-1": entries}
		h.replayer.mappingDB = &shMappingDB{
			prMappingDB: h.mappings,
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: entries}},
				Worker:    []models.MockEntry{{Name: "mock-bg-1", Kind: string(models.Mongo)}},
			},
		}
		h.replayer.hookImpl = prHooks{
			wrongBody: map[string]bool{"test-1": true},
			consumed: []models.MockState{
				{Name: "mock-own-1", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
				{Name: "mock-bg-1", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
			},
		}

		_ = h.run(t)
		got := resultOf(t, h, "test-1")
		if got.Status != models.TestStatusFailed {
			t.Fatalf("status = %s, want FAILED", got.Status)
		}
		failures := h.replayer.mockMismatchFailures.GetFailures()
		if len(failures) != 1 {
			t.Fatalf("expected 1 mockMismatchFailure, got %d: %+v", len(failures), failures)
		}
		wantExp := []string{"mock-own-1", "mock-own-2"}
		wantAct := []string{"mock-own-1"}
		if !reflect.DeepEqual(failures[0].ExpectedMocks, wantExp) || !reflect.DeepEqual(failures[0].ActualMocks, wantAct) {
			t.Fatalf("mockMismatchFailures = exp:%v act:%v, want exp:%v act:%v",
				failures[0].ExpectedMocks, failures[0].ActualMocks, wantExp, wantAct)
		}
	})
}

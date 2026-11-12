// Tests for the runner package (package name: integration).
//
// loadMappingsForSet is the regression surface here. The fix changed its
// behaviour when mappings.yaml is absent on disk: previously it errored
// with "no mock mappings found for test set %q" and the entire runner
// blew up; the fix returns empty maps + nil error so the runner can
// degrade to "use every mock in the set" (DisableMapping-equivalent).
// This file pins all three branches of that function so a future refactor
// can't accidentally reintroduce the error-on-empty behaviour or break
// the meaningful-mappings success path or the genuine I/O-error
// propagation path.
package integration

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// stubMappingDB is a hand-rolled minimal stub. We don't pull in testify
// mocks because the surface is a single method and the call shape is
// already deterministic per sub-test.
type stubMappingDB struct {
	mappings      map[string][]models.MockEntry
	startup       []models.MockEntry
	startupErr    error
	hasMeaningful bool
	err           error
	calledWithSet string
	callCount     int
}

func (s *stubMappingDB) Get(_ context.Context, testSetID string) (map[string][]models.MockEntry, bool, error) {
	s.callCount++
	s.calledWithSet = testSetID
	return s.mappings, s.hasMeaningful, s.err
}

func (s *stubMappingDB) GetStartup(_ context.Context, _ string) ([]models.MockEntry, error) {
	return s.startup, s.startupErr
}

// TestLoadMappingsForSet_MissingFile pins the tolerance for an absent
// mappings.yaml. The fix replaced an outright error with "return empty
// maps so the runner falls back to loading every mock in the set"; this
// is the path every OSS-shape sandbox replay takes (no mappings.yaml on
// disk, just keploy/<set>/{tests,mocks.yaml}).
func TestLoadMappingsForSet_MissingFile(t *testing.T) {
	t.Run("missing_returns_empty_maps_no_error", func(t *testing.T) {
		// hasMeaningful = false, no error — the "no mappings on disk"
		// shape MappingDB returns when mappings.yaml is absent.
		stub := &stubMappingDB{
			mappings:      map[string][]models.MockEntry{},
			hasMeaningful: false,
			err:           nil,
		}
		r := &Runner{mappingDB: stub}

		mappings, mocksThatHaveMappings, mocksWeNeed, _, err := r.loadMappingsForSet(context.Background(), "set-without-mappings")

		if err != nil {
			t.Fatalf("expected nil error when mappings absent, got %v", err)
		}
		if mappings == nil {
			t.Fatalf("expected non-nil empty mappings map, got nil (downstream code dereferences this directly)")
		}
		if len(mappings) != 0 {
			t.Fatalf("expected empty mappings map, got %d entries", len(mappings))
		}
		if mocksThatHaveMappings == nil {
			t.Fatalf("expected non-nil empty mocksThatHaveMappings map, got nil")
		}
		if len(mocksThatHaveMappings) != 0 {
			t.Fatalf("expected empty mocksThatHaveMappings map, got %d entries", len(mocksThatHaveMappings))
		}
		if mocksWeNeed == nil {
			t.Fatalf("expected non-nil empty mocksWeNeed map, got nil")
		}
		if len(mocksWeNeed) != 0 {
			t.Fatalf("expected empty mocksWeNeed map, got %d entries", len(mocksWeNeed))
		}
		if stub.calledWithSet != "set-without-mappings" {
			t.Fatalf("Get was called with %q, expected %q", stub.calledWithSet, "set-without-mappings")
		}
	})

	t.Run("meaningful_mappings_returned_unchanged", func(t *testing.T) {
		// Success path — confirms the fix did NOT break the path
		// that loads real mappings.yaml content.
		want := map[string][]models.MockEntry{
			"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "mock-2", Kind: "Postgres"}},
			"test-2": {{Name: "mock-2", Kind: "Postgres"}, {Name: "mock-3", Kind: "MySQL"}},
		}
		stub := &stubMappingDB{
			mappings:      want,
			hasMeaningful: true,
			err:           nil,
		}
		r := &Runner{mappingDB: stub}

		mappings, mocksThatHaveMappings, mocksWeNeed, _, err := r.loadMappingsForSet(context.Background(), "set-with-mappings")
		if err != nil {
			t.Fatalf("expected nil error on success, got %v", err)
		}
		if len(mappings) != 2 {
			t.Fatalf("expected 2 test entries in mappings, got %d", len(mappings))
		}
		// UNION of mock names across both tests = {mock-1, mock-2, mock-3}.
		expectedUnion := map[string]bool{"mock-1": true, "mock-2": true, "mock-3": true}
		if len(mocksThatHaveMappings) != len(expectedUnion) {
			t.Fatalf("mocksThatHaveMappings: expected %d entries, got %d",
				len(expectedUnion), len(mocksThatHaveMappings))
		}
		for name := range expectedUnion {
			if !mocksThatHaveMappings[name] {
				t.Fatalf("mocksThatHaveMappings missing %q", name)
			}
			if !mocksWeNeed[name] {
				t.Fatalf("mocksWeNeed missing %q", name)
			}
		}
	})

	t.Run("genuine_error_is_propagated", func(t *testing.T) {
		// An I/O error from the underlying mappingDB.Get MUST NOT be
		// swallowed by the tolerance branch — only the
		// hasMeaningful=false path should degrade gracefully.
		sentinel := errors.New("disk read failed")
		stub := &stubMappingDB{
			mappings:      nil,
			hasMeaningful: false,
			err:           sentinel,
		}
		r := &Runner{mappingDB: stub}

		mappings, mthm, mwn, _, err := r.loadMappingsForSet(context.Background(), "set-broken")
		if err == nil {
			t.Fatalf("expected non-nil error when mappingDB.Get returns err, got nil")
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected returned error to wrap %v, got %v", sentinel, err)
		}
		if mappings != nil || mthm != nil || mwn != nil {
			t.Fatalf("expected nil maps on error path, got mappings=%v mthm=%v mwn=%v",
				mappings, mthm, mwn)
		}
	})

	t.Run("nil_mappingDB_returns_error", func(t *testing.T) {
		// Sanity check: keeps the nil-guard at the top of
		// loadMappingsForSet honoured. If a future refactor removes
		// that guard a nil-pointer panic would silently take down the
		// runner; better to surface the configuration error.
		r := &Runner{mappingDB: nil}
		_, _, _, _, err := r.loadMappingsForSet(context.Background(), "anything")
		if err == nil {
			t.Fatalf("expected error when mappingDB is nil, got nil")
		}
	})
}

// The step's mismatch report leaves out what the replayer's dependency
// assertion leaves out (models.ExcludedFromDependencyAssertion), from both
// lists: DNS, and connection failures, which nothing consumes at replay yet
// and which would otherwise always be reported expected-but-not-consumed.
func TestMockMismatchReportLeavesOutWhatTheAssertionDoes(t *testing.T) {
	r := &Runner{logger: zap.NewNop(), instrumentation: &paramsInstr{}}
	setup := &testSetSetup{mockKindByName: map[string]models.Kind{
		"mock-4": models.ConnectionFailure, // kind known only from the loaded mock
	}}
	got := r.checkMockMismatches(setup, []MockRef{
		{Name: "mock-1", Kind: "Http"},
		{Name: "mock-2", Kind: "DNS"},
		{Name: "mock-3", Kind: "ConnectionFailure"},
		{Name: "mock-4"},
	}, []models.MockState{
		{Name: "mock-1", Kind: models.HTTP},
		{Name: "mock-5", Kind: models.ConnectionFailure},
	})
	want := &MockMismatch{
		ExpectedMocks: []MockRef{{Name: "mock-1", Kind: "Http"}},
		ConsumedMocks: []MockRef{{Name: "mock-1", Kind: "Http"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch report = %+v, want %+v", got, want)
	}
}

// setupInstr is an agent connection that accepts every set-up call, and
// keeps what it was asked to store.
type setupInstr struct {
	paramsInstr
	stored   []*models.Mock
	outgoing []models.OutgoingOptions
}

func (s *setupInstr) MockOutgoing(_ context.Context, opts models.OutgoingOptions) error {
	s.outgoing = append(s.outgoing, opts)
	return nil
}
func (s *setupInstr) StoreMocks(_ context.Context, f, u []*models.Mock) error {
	s.stored = append(append(s.stored, f...), u...)
	return nil
}
func (*setupInstr) MakeAgentReadyForDockerCompose(context.Context) error { return nil }
func (*setupInstr) NotifyGracefulShutdown(context.Context) error         { return nil }
func (*setupInstr) WatchAgentStart(context.Context) <-chan error         { return nil }

// skippingMockDB reads every pool in one pass (TestSetMocksReader); its
// decoders skipped the named documents and read the filtered mocks.
type skippingMockDB struct {
	skipped  map[string]models.Kind
	filtered []*models.Mock
}

func (*skippingMockDB) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (*skippingMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (m *skippingMockDB) GetTestSetMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) (models.TestSetMocks, error) {
	return models.TestSetMocks{Skipped: m.skipped, Filtered: m.filtered}, nil
}

// The runner stores on the agent what the agent holds: never a connection
// failure, which this keploy does not replay (models.AgentBound).
func TestTheRunnerStoresNoConnectionFailureOnTheAgent(t *testing.T) {
	instr := &setupInstr{}
	r := &Runner{
		logger:          zap.NewNop(),
		instrumentation: instr,
		mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
		mockDB: &skippingMockDB{filtered: []*models.Mock{
			{Name: "mock-0", Kind: models.ConnectionFailure}, {Name: "mock-1", Kind: models.HTTP},
		}},
	}
	setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
	if err != nil {
		t.Fatalf("setupTestSet: %v", err)
	}
	defer setup.cleanup()
	if len(instr.stored) != 1 || instr.stored[0].Name != "mock-1" {
		t.Fatalf("stored %v on the agent; want only mock-1", instr.stored)
	}
	if setup.mockKindByName["mock-0"] != models.ConnectionFailure {
		t.Fatal("the connection failure's kind is still known, for the mismatch report")
	}
}

// The runner hands the agent keploy.yml's matching noise in every bucket the
// agent's mock matchers read, url included: test.globalNoise.url masks the
// part of a path or query value that changes per run, and reaches the HTTP
// matcher only through OutgoingOptions.NoiseConfig. A bucket no matcher reads
// stays out.
func TestTheRunnerHandsTheAgentEveryMatchingNoiseBucket(t *testing.T) {
	instr := &setupInstr{}
	r := &Runner{
		logger:          zap.NewNop(),
		instrumentation: instr,
		mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
		mockDB:          &skippingMockDB{},
		globalNoise: models.GlobalNoise{
			"header":      {"x-request-id": {}},
			"body":        {"eventTs": {}},
			"requestbody": {"nonce": {}},
			"url":         {"/orders/": {`[0-9]+`}},
			"status":      {"code": {}},
		},
	}
	setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
	if err != nil {
		t.Fatalf("setupTestSet: %v", err)
	}
	defer setup.cleanup()
	if len(instr.outgoing) != 1 {
		t.Fatalf("MockOutgoing called %d times; want 1", len(instr.outgoing))
	}
	want := map[string]map[string][]string{
		"header":      {"x-request-id": {}},
		"body":        {"eventTs": {}},
		"requestbody": {"nonce": {}},
		"url":         {"/orders/": {`[0-9]+`}},
	}
	if got := instr.outgoing[0].NoiseConfig; !reflect.DeepEqual(got, want) {
		t.Fatalf("NoiseConfig = %v, want %v", got, want)
	}
}

// The runner holds a small entry per mock, not the set: from the YAML store it
// stores on the agent each mock with its payload left in the set's mocks.yaml,
// which the agent client reads back as it sends the mock (models.Mock.WithPayload).
func TestTheRunnerStoresMocksWithTheirPayloadsLeftOnDisk(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "") // gob would write, and read, a mocks.gob instead
	dir := t.TempDir()
	ys := mockdb.New(zap.NewNop(), dir, "mocks")
	ts := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	for i, typ := range []string{"config", "mocks"} {
		m := &models.Mock{
			Version: models.GetVersion(),
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": typ},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://example.com/" + typ, ProtoMajor: 1, ProtoMinor: 1},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Body: `{"pool":"` + typ + `"}`},
				ReqTimestampMock: ts.Add(time.Duration(i) * time.Second),
				ResTimestampMock: ts.Add(time.Duration(i)*time.Second + time.Millisecond),
			},
		}
		if err := ys.InsertMock(context.Background(), m, "test-set-0"); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
	}

	instr := &setupInstr{}
	r := &Runner{
		logger:          zap.NewNop(),
		instrumentation: instr,
		mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
		mockDB:          ys,
	}
	setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
	if err != nil {
		t.Fatalf("setupTestSet: %v", err)
	}
	defer setup.cleanup()

	if len(instr.stored) != 2 {
		t.Fatalf("stored %d mocks on the agent; want both of the set's", len(instr.stored))
	}
	for _, m := range instr.stored {
		if !m.PayloadOnDisk() || m.Spec.HTTPResp != nil {
			t.Fatalf("%s was stored with its payload in memory; the runner holds a small entry per mock", m.Name)
		}
		sent, err := m.WithPayload(context.Background())
		if err != nil {
			t.Fatalf("reading %s's payload back: %v", m.Name, err)
		}
		want := `{"pool":"` + m.Spec.Metadata["type"] + `"}`
		if sent.Spec.HTTPResp == nil || sent.Spec.HTTPResp.Body != want {
			t.Fatalf("%s read back %+v; want the recorded body %s", m.Name, sent.Spec.HTTPResp, want)
		}
	}
}

// A mapping entry recorded without a kind can name a connection failure the
// decoders skipped; the step's report leaves it out by that document's kind,
// as the replayer does. Other skipped kinds are not looked up.
func TestASkippedConnectionFailureIsLeftOutOfTheStepReportByName(t *testing.T) {
	r := &Runner{
		logger:          zap.NewNop(),
		instrumentation: &setupInstr{},
		mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
		mockDB:          &skippingMockDB{skipped: map[string]models.Kind{"cf-kindless": models.ConnectionFailure, "mock-acme": "Acme-Queue"}},
	}
	setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
	if err != nil {
		t.Fatalf("setupTestSet: %v", err)
	}
	defer setup.cleanup()
	got := r.checkMockMismatches(setup, []MockRef{{Name: "mock-1", Kind: "Http"}, {Name: "cf-kindless"}, {Name: "mock-acme"}}, nil)
	want := []MockRef{{Name: "mock-1", Kind: "Http"}, {Name: "mock-acme"}}
	if !reflect.DeepEqual(got.ExpectedMocks, want) {
		t.Fatalf("expected side = %+v, want %+v", got.ExpectedMocks, want)
	}
	// The step's expected entries label a kind-less entry from the loaded
	// kinds; a skipped document of another kind keeps no label, as before.
	setup.mappings = map[string][]models.MockEntry{"step-1": {{Name: "mock-acme"}}}
	if entries := expectedEntriesForTest(setup, "step-1"); len(entries) != 1 || entries[0].Kind != "" {
		t.Fatalf("expected entries = %+v; the skipped mock-acme must keep no kind", entries)
	}
}

// wholeMappingDB is stubMappingDB that also reads the whole mapping.
type wholeMappingDB struct {
	*stubMappingDB
	mapping *models.Mapping
	err     error
}

func (w *wholeMappingDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return w.mapping, w.err
}

// The runner serves a set's per-process warm-ups as keploy test does: the
// marked worker entries only, none under the proven switch or with mapping
// disabled, and none, without failing the run, when the mapping cannot be
// read whole.
func TestWarmupNamesAreTheMarkedWorkerEntries(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	db := &wholeMappingDB{stubMappingDB: &stubMappingDB{}, mapping: &models.Mapping{Worker: []models.MockEntry{
		{Name: "jwks-1", Reason: models.PerProcessWarmupReason},
		{Name: "poll-1"},
		{Name: "jwks-2", Reason: models.PerProcessWarmupReason},
	}}}
	r := &Runner{logger: zap.NewNop(), mappingDB: db}
	if got := r.warmupNames(context.Background(), "set"); !reflect.DeepEqual(got, []string{"jwks-1", "jwks-2"}) {
		t.Fatalf("warm-ups %v; want [jwks-1 jwks-2]", got)
	}
	if got := (&Runner{logger: zap.NewNop(), mappingDB: &stubMappingDB{}}).warmupNames(context.Background(), "set"); got != nil {
		t.Fatalf("a store that cannot read the whole mapping gave %v", got)
	}
	db.err = errors.New("boom")
	if got := r.warmupNames(context.Background(), "set"); got != nil {
		t.Fatalf("a failed read gave %v", got)
	}
	db.err = nil
	disabled := &Runner{logger: zap.NewNop(), mappingDB: db, config: &config.Config{DisableMapping: true}}
	if got := disabled.warmupNames(context.Background(), "set"); got != nil {
		t.Fatalf("with mapping disabled: %v", got)
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if got := r.warmupNames(context.Background(), "set"); got != nil {
		t.Fatalf("under the proven switch: %v", got)
	}
}

// The runner stores a set's per-process warm-ups on the agent as session
// mocks, once each, and names them for every step: whichever step reaches a
// cold process is answered by them.
func TestTheRunnerStoresTheWarmupsAsSessionMocks(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	instr := &setupInstr{}
	perTest := func(name string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP,
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	r := &Runner{
		logger:          zap.NewNop(),
		instrumentation: instr,
		mappingDB: &wholeMappingDB{
			stubMappingDB: &stubMappingDB{mappings: map[string][]models.MockEntry{"step-1": {{Name: "jwks-1"}, {Name: "own-1"}}}, hasMeaningful: true},
			mapping:       &models.Mapping{Worker: []models.MockEntry{{Name: "jwks-1", Reason: models.PerProcessWarmupReason}}},
		},
		mockDB: &skippingMockDB{filtered: []*models.Mock{perTest("jwks-1"), perTest("own-1")}},
	}
	setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
	if err != nil {
		t.Fatalf("setupTestSet: %v", err)
	}
	defer setup.cleanup()
	var jwks []*models.Mock
	for _, m := range instr.stored {
		if m.Name == "jwks-1" {
			jwks = append(jwks, m)
		}
	}
	if len(jwks) != 1 || jwks[0].TestModeInfo.Lifetime != models.LifetimeSession || jwks[0].TestModeInfo.IsFiltered {
		t.Fatalf("stored jwks-1 %d times (%+v); want once, a session mock", len(jwks), jwks)
	}
	if !slices.Contains(setup.startupMockNames, "jwks-1") {
		t.Fatalf("names for every step %v; want jwks-1 among them", setup.startupMockNames)
	}
}

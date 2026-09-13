package proxy

import (
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// fakeMockDb is a minimal MockMemDb: the per-test read methods return a fixed
// set; every other method is the promoted (nil) interface and must not be called
// by these tests.
type fakeMockDb struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

func (f *fakeMockDb) GetFilteredMocks() ([]*models.Mock, error)         { return f.mocks, nil }
func (f *fakeMockDb) GetFilteredMocksInWindow() ([]*models.Mock, error) { return f.mocks, nil }
func (f *fakeMockDb) GetPerTestMocksInWindow() ([]*models.Mock, error)  { return f.mocks, nil }
func (f *fakeMockDb) GetSessionMocks() ([]*models.Mock, error)          { return f.mocks, nil }
func (f *fakeMockDb) GetUnFilteredMocks() ([]*models.Mock, error)       { return f.mocks, nil }
func (f *fakeMockDb) GetSessionScopedMocks() ([]*models.Mock, error)    { return f.mocks, nil }

func scopedNames(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func TestScopedMockDbFiltersReads(t *testing.T) {
	// m1,m2,m3 belong to tests (in the universe); "shared" belongs to no test.
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "m1"}, {Name: "m2"}, {Name: "m3"}, {Name: "shared"}}}
	universe := map[string]struct{}{"m1": {}, "m2": {}, "m3": {}}

	// Worker allowed m1,m3: it sees m1,m3 (its test) + "shared" (unmapped), but
	// NOT m2 (another test's mock). Applies to per-test AND session tiers.
	s := &scopedMockDb{MockMemDb: db, allow: map[string]struct{}{"m1": {}, "m3": {}}, universe: universe}
	for _, get := range []func() ([]*models.Mock, error){
		s.GetPerTestMocksInWindow, s.GetFilteredMocks, s.GetFilteredMocksInWindow,
		s.GetSessionMocks, s.GetUnFilteredMocks, s.GetSessionScopedMocks,
	} {
		got, err := get()
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"m1", "m3", "shared"}, scopedNames(got),
			"worker sees its own test's mocks plus unmapped shared mocks, never another test's")
	}

	// A nil universe (no mappings pushed) is a passthrough — never hide anything.
	pass := &scopedMockDb{MockMemDb: db, allow: map[string]struct{}{"m1": {}}, universe: nil}
	got, err := pass.GetSessionMocks()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"m1", "m2", "m3", "shared"}, scopedNames(got))
}

func TestScopedForResolvesWorkerAndFallsBack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scopedFor's process-tree walk reads /proc (linux only)")
	}
	db := &fakeMockDb{}
	self := uint32(os.Getpid())

	// Empty registry: fast path, no /proc walk, returns the bare manager.
	p := &Proxy{}
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(self, db), "no scopes ⇒ whole pool")

	// A registered worker PID: the call is scoped to its allowlist.
	p.SetWorkerScope(self, []string{"only-this"})
	require.IsType(t, &scopedMockDb{}, p.scopedFor(self, db), "own PID resolves to a scoped view")

	// PID 0 (unknown origin) and an unregistered/dead PID ⇒ whole pool.
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(0, db))
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(4000000000, db))

	// Cleared ⇒ back to the whole pool.
	p.ClearWorkerScope(self)
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(self, db))
}

func TestScopedForWalksUpProcessTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-tree walk is linux only")
	}
	db := &fakeMockDb{}
	// Register the PARENT of this process; this process's call must resolve up to
	// it (models a worker whose child/grandchild opened the socket).
	p := &Proxy{}
	p.SetWorkerScope(uint32(os.Getppid()), []string{"m"})
	require.IsType(t, &scopedMockDb{}, p.scopedFor(uint32(os.Getpid()), db),
		"a call from a descendant resolves up to the registered worker")

	p.ClearAllWorkerScopes()
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(uint32(os.Getpid()), db))
}

func TestPpidFromStat(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc only on linux")
	}
	ppid, ok := ppidFromStat(uint32(os.Getpid()))
	require.True(t, ok)
	require.Equal(t, uint32(os.Getppid()), ppid)

	_, ok = ppidFromStat(4000000000) // no such pid
	require.False(t, ok)
}

// SetWorkerScope with an empty name list clears the entry (no-mapping ⇒ suite).
func TestSetWorkerScopeEmptyClears(t *testing.T) {
	p := &Proxy{}
	p.SetWorkerScope(42, []string{"a"})
	p.SetWorkerScope(42, nil)
	p.workerScopeMu.RLock()
	_, present := p.workerScope[42]
	p.workerScopeMu.RUnlock()
	require.False(t, present, "empty allowlist clears the worker's scope")
}

// The wrapper embeds the MockMemDb INTERFACE, so anything a parser reaches for
// by type assertion but that is not in that interface is silently erased by the
// wrap. Every kind-aware parser then falls to a legacy branch — and in
// `keploy mock replay`, the only mode where worker scoping exists, the startup
// tier is the whole pool, so that branch reads nothing.
func TestScopedMockDb_DoesNotEraseTheManagersCapabilities(t *testing.T) {
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mgr.Close()

	type revSrc interface{ Revision() uint64 }
	type revKind interface{ RevisionByKind(models.Kind) uint64 }
	type kindAware interface {
		GetFilteredMocksByKind(models.Kind) ([]*models.Mock, error)
		GetUnFilteredMocksByKind(models.Kind) ([]*models.Mock, error)
	}

	var scoped interface{} = &scopedMockDb{MockMemDb: mgr}
	if _, ok := scoped.(revSrc); !ok {
		t.Error("Revision erased by the wrap: consumers fall back to rebuilding on every call")
	}
	if _, ok := scoped.(revKind); !ok {
		t.Error("RevisionByKind erased by the wrap")
	}
	if _, ok := scoped.(kindAware); !ok {
		t.Error("the by-kind readers are erased by the wrap: kind-aware parsers take their " +
			"legacy branch, which cannot read the startup tier")
	}
}

// The startup tier is filtered like every other read tier. It sounds shared,
// but staging puts the whole per-test slice into it, and in `keploy mock replay`
// it is the entire pool — so leaving it unfiltered let one worker read, and
// consume, another worker's mocks.
func TestScopedMockDb_StartupTierIsScopedToTheWorker(t *testing.T) {
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mgr.Close()

	a := newMockForTest("mock-A", time.Now().Add(-time.Hour), models.LifetimePerTest)
	b := newMockForTest("mock-B", time.Now().Add(-time.Hour), models.LifetimePerTest)
	mgr.SetMocksWithWindow([]*models.Mock{a, b}, nil, models.BaseTime, time.Now())

	universe := map[string]struct{}{"mock-A": {}, "mock-B": {}}
	workerA := &scopedMockDb{
		MockMemDb: mgr,
		allow:     map[string]struct{}{"mock-A": {}},
		universe:  universe,
	}

	startup, err := workerA.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if containsMockNamed(startup, "mock-B") {
		t.Fatal("worker A can see worker B's mock through the startup tier; it can also " +
			"consume it, deleting another worker's recording")
	}
	if !containsMockNamed(startup, "mock-A") {
		t.Fatal("worker A cannot see its own mock")
	}
}

// A CORS preflight recorded inside test_A's window is attributed to test_A in
// mappings.yaml, because correlateScopes buckets by request timestamp alone and
// never looks at Lifetime. That puts its name in `universe`. A worker running
// test_B does not own it, so name-only filtering drops it — and test_B's
// browser preflight then finds no mock and fails CORS with net::ERR_FAILED.
//
// A preflight's recorded response is the endpoint's allow-policy, not test_A's
// property, so it must survive. Same for a MySQL connection-alive command.
func TestScopedMockDb_KeepsASharedPreflightAnotherTestOwns(t *testing.T) {
	preflight := &models.Mock{
		Name: "preflight-options-query",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq: &models.HTTPReq{
				Method: models.Method("OPTIONS"),
				URL:    "/query",
				Header: map[string]string{"Access-Control-Request-Method": "POST"},
			},
			HTTPResp: &models.HTTPResp{StatusCode: 204},
		},
	}
	preflight.DeriveLifetime()
	if preflight.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatalf("precondition: a preflight should derive to session, got %v", preflight.TestModeInfo.Lifetime)
	}
	if !preflight.IsSharedAcrossTests() {
		t.Fatalf("precondition: a preflight is a rule-1 override")
	}

	dataMock := &models.Mock{Name: "test-A-post-query", Kind: models.HTTP}
	db := &fakeMockDb{mocks: []*models.Mock{preflight, dataMock}}

	// mappings.yaml says test_A owns both. This worker is running test_B.
	workerB := &scopedMockDb{
		MockMemDb: db,
		allow:     map[string]struct{}{"test-B-get-query": {}},
		universe: map[string]struct{}{
			"preflight-options-query": {},
			"test-A-post-query":       {},
		},
	}

	got, err := workerB.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks: %v", err)
	}
	names := scopedNames(got)
	if !slices.Contains(names, "preflight-options-query") {
		t.Fatalf("a CORS preflight is the endpoint's allow-policy and must be visible to every worker; got %v", names)
	}
	if slices.Contains(names, "test-A-post-query") {
		t.Fatalf("test A's data mock must STILL be hidden — the fix is not 'exempt every session mock'; got %v", names)
	}
}

// CONTROL. An untagged legacy HTTP mock derives to session via rule 4, but it
// is ordinary data-plane traffic owned by one test. If keep() exempted mocks by
// Lifetime instead of by derivation reason, worker isolation would become a
// no-op for every pre-tag recording. This must pass before AND after.
func TestScopedMockDb_StillHidesATierPromotedSessionMock(t *testing.T) {
	legacy := &models.Mock{
		Name: "test-A-untagged-get",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: models.Method("GET"), URL: "/items"},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		},
	}
	legacy.DeriveLifetime()
	if legacy.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatalf("precondition: rule 4 promotes untagged HTTP to session, got %v", legacy.TestModeInfo.Lifetime)
	}
	if legacy.IsSharedAcrossTests() {
		t.Fatalf("precondition: session-by-tiering is NOT session-by-semantics")
	}

	db := &fakeMockDb{mocks: []*models.Mock{legacy}}
	workerB := &scopedMockDb{
		MockMemDb: db,
		allow:     map[string]struct{}{"test-B-get-query": {}},
		universe:  map[string]struct{}{"test-A-untagged-get": {}},
	}
	got, err := workerB.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a tier-promoted session mock owned by another test must stay hidden; got %v", scopedNames(got))
	}
}

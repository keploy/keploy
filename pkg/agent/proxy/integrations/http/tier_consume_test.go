package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// tierDb is a MockMemDb stub that reports each tier SEPARATELY, so a test can
// assert which tier a consume was issued against. The legacy GetSessionMocks
// union shim is implemented faithfully (startup ∪ session) so a parser reading
// either the shim or the strict accessors sees the same candidates.
type tierDb struct {
	perTest []*models.Mock
	startup []*models.Mock
	session []*models.Mock

	deleteFilteredCalls int
	deleteStartupCalls  int
	updateUnFilteredHit int
	markUsedCalls       int

	deleteFilteredReturn bool
	deleteStartupReturn  bool

	// onDeleteFiltered runs before DeleteFilteredMock returns, so a test can
	// model another connection having won the race and drained the pool.
	onDeleteFiltered func()
}

func (d *tierDb) GetPerTestMocksInWindow() ([]*models.Mock, error) { return d.perTest, nil }
func (d *tierDb) GetStartupMocks() ([]*models.Mock, error)         { return d.startup, nil }
func (d *tierDb) GetSessionScopedMocks() ([]*models.Mock, error)   { return d.session, nil }
func (d *tierDb) GetSessionMocks() ([]*models.Mock, error) {
	out := append([]*models.Mock{}, d.startup...)
	return append(out, d.session...), nil
}
func (d *tierDb) DeleteFilteredMock(_ models.Mock) bool {
	d.deleteFilteredCalls++
	if d.onDeleteFiltered != nil {
		d.onDeleteFiltered()
	}
	return d.deleteFilteredReturn
}
func (d *tierDb) DeleteStartupMock(_ models.Mock) bool {
	d.deleteStartupCalls++
	return d.deleteStartupReturn
}
func (d *tierDb) UpdateUnFilteredMock(_ *models.Mock, _ *models.Mock) bool {
	d.updateUnFilteredHit++
	return false
}
func (d *tierDb) MarkMockAsUsed(_ models.Mock) bool { d.markUsedCalls++; return true }

func (d *tierDb) GetUnFilteredMocks() ([]*models.Mock, error) { return d.session, nil }
func (d *tierDb) GetFilteredMocks() ([]*models.Mock, error)   { return d.perTest, nil }
func (d *tierDb) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	return d.perTest, nil
}
func (d *tierDb) DeleteUnFilteredMock(_ models.Mock) bool   { return false }
func (d *tierDb) GetMySQLCounts() (total, config, data int) { return 0, 0, 0 }
func (d *tierDb) SetCurrentTestWindow(_, _ time.Time)       {}
func (d *tierDb) IsTestWindowActive() bool                  { return false }
func (d *tierDb) GetStartupMocksByKind(_ models.Kind) ([]*models.Mock, error) {
	return d.startup, nil
}
func (d *tierDb) HasFirstTestFired() bool                             { return false }
func (d *tierDb) FirstTestWindowStart() time.Time                     { return time.Time{} }
func (d *tierDb) WindowSnapshot() models.WindowSnapshot               { return models.WindowSnapshot{} }
func (d *tierDb) CurrentTestWindow() (time.Time, time.Time)           { return time.Time{}, time.Time{} }
func (d *tierDb) GetConnectionMocks(_ string) ([]*models.Mock, error) { return nil, nil }
func (d *tierDb) SessionMockHitCounts() map[string]uint64             { return nil }

func perTestMock(name, path string) *models.Mock {
	m := httpMock(name, "GET", "http://api"+path)
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

func getReq(path string) *req {
	return &req{method: "GET", url: &url.URL{Path: path}, header: http.Header{}}
}

// A mock READ from the startup tier must be consumed from the startup tier,
// and nothing else may be touched.
//
// The startup tier is where per-test mocks actually live on the
// `keploy mock replay` path (every staging call passes AfterTime = BaseTime,
// so SetMocksWithWindow routes the whole per-test slice into `startup`).
//
// DeleteFilteredMock is stubbed to return TRUE, which is what a wrong-tier
// coordinate collision looks like from the caller: the per-test tree orders on
// (SortOrder, ID) with no IsFiltered check and both are per-pool 0-based
// indices, so a speculative delete can find a DIFFERENT mock at the same
// coordinates, evict it, and report success. A chain that probes
// DeleteFilteredMock first therefore destroys an unrelated mock AND leaves the
// served one un-consumed — so this test fails on the probe-chain
// implementation and passes on the tier-targeted one.
func TestMatch_StartupTierMockConsumedFromStartupTier(t *testing.T) {
	h := newHTTP()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db := &tierDb{
		startup:              []*models.Mock{perTestMock("mock-0", "/items")},
		deleteFilteredReturn: true, // a wrong-tier eviction "succeeds"
		deleteStartupReturn:  true,
	}

	ok, got, _, err := h.match(ctx, getReq("/items"), db, nil, nil, nil, true, false, false)
	if err != nil || !ok || got == nil {
		t.Fatalf("expected the startup-tier mock to match: ok=%v mock=%v err=%v", ok, got, err)
	}
	if db.deleteStartupCalls != 1 {
		t.Errorf("expected exactly 1 DeleteStartupMock call, got %d", db.deleteStartupCalls)
	}
	if db.deleteFilteredCalls != 0 {
		t.Errorf("a startup-tier mock must never be probed against the per-test tree: "+
			"DeleteFilteredMock called %d time(s)", db.deleteFilteredCalls)
	}
	if db.updateUnFilteredHit != 0 {
		t.Errorf("a startup-tier mock must not be probed against the session tree: "+
			"UpdateUnFilteredMock called %d time(s)", db.updateUnFilteredHit)
	}
}

// A per-test mock read from the per-test tier must be consumed from the
// per-test tier, with no cross-tier probing.
func TestMatch_PerTestTierMockConsumedFromPerTestTier(t *testing.T) {
	h := newHTTP()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db := &tierDb{
		perTest:              []*models.Mock{perTestMock("mock-0", "/items")},
		deleteFilteredReturn: true,
		deleteStartupReturn:  true,
	}

	ok, _, _, err := h.match(ctx, getReq("/items"), db, nil, nil, nil, true, false, false)
	if err != nil || !ok {
		t.Fatalf("expected the per-test mock to match: ok=%v err=%v", ok, err)
	}
	if db.deleteFilteredCalls != 1 || db.deleteStartupCalls != 0 {
		t.Errorf("expected exactly one DeleteFilteredMock and no DeleteStartupMock, got %d/%d",
			db.deleteFilteredCalls, db.deleteStartupCalls)
	}
}

// A failed consume means the LOST-CONSUME RACE: another connection scored the
// same mock, its delete won, and this one must re-match against the shrunk
// pool rather than serve a mock that is already gone. mongo/v2 documents the
// identical contract at decode.go:1024.
//
// The stub models exactly that: DeleteFilteredMock returns false and drains
// the pool, as the winner's delete would have. Correct behaviour is a MISS.
// An implementation that floors the return with MarkMockAsUsed reports a match
// instead — i.e. it double-serves — so this test fails on the floored
// implementation.
//
// The context carries a deadline on purpose: a regression that reinstates the
// unbounded retry must FAIL here rather than hang the suite.
func TestMatch_LostConsumeRaceMissesInsteadOfDoubleServing(t *testing.T) {
	h := newHTTP()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db := &tierDb{
		perTest:              []*models.Mock{perTestMock("mock-0", "/items")},
		deleteFilteredReturn: false,
		deleteStartupReturn:  false,
	}
	db.onDeleteFiltered = func() { db.perTest = nil }

	ok, got, diag, err := h.match(ctx, getReq("/items"), db, nil, nil, nil, true, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("a lost consume race must not serve the mock; got a match on %v", got)
	}
	if diag == nil || diag.phase != models.MatchPhaseNoMocks {
		t.Errorf("expected the retry to re-read the shrunk pool and report no_mocks, got diag=%+v", diag)
	}
	if db.markUsedCalls != 0 {
		t.Errorf("MarkMockAsUsed must not be used as a totality floor; called %d time(s)", db.markUsedCalls)
	}
}

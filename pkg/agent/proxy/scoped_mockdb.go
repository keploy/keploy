package proxy

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
	"go.keploy.io/server/v3/utils"
)

// Per-PID (worker-keyed) mock scoping — "Design A".
//
// Parallel test runners (Playwright, jest, pytest-xdist, `go test` -parallel)
// spawn WORKER PROCESSES that each run their tests sequentially. The scope API
// (POST /agent/scope/begin|end) lets a worker mark its current test so replay
// serves only that test's recorded mocks. With a single global filter, two
// workers' begin/end calls stomp each other; keying the filter by the worker's
// PID instead lets them run concurrently without a shared "current scope".
//
// A worker self-reports its PID in the scope call (ScopeReq.Pid — e.g. Node
// process.pid), which registers an allowlist of that test's mock names on the
// proxy (SetWorkerScope). A connection's origin PID (OutgoingOptions.SrcPid,
// from the eBPF redirect map) is resolved up the /proc process tree once, when
// it opens; each read on it is then filtered by the allowlist of the nearest
// worker on that chain registered at that moment, so the connection follows
// its worker from test to test. Connections from unregistered process trees
// (and every connection when no worker has scoped) get the whole pool — so
// suite-level and single-worker sequential scoping behave exactly as before.
//
// Scope of the isolation: this filters VISIBILITY (which per-test mocks a call
// can see), over the one shared pool. Per-test mocks are per-test-named in
// mappings.yaml, so worker allowlists are disjoint in practice and consumption
// (DeleteFilteredMock) does not collide.
//
// The STARTUP tier is filtered too, and that is not obvious: it sounds like a
// shared bootstrap tier, but SetMocksWithWindow puts the whole per-test slice
// into it during BaseTime staging — and in `keploy mock replay`, the one mode
// where worker scoping exists, the pool is staged once at BaseTime and never
// re-partitioned, so the startup tier IS the whole pool. Leaving it unfiltered
// let a worker read, and DELETE, another worker's mocks. The file used to say
// startup passed through unfiltered while also filtering GetSessionMocks, which
// is defined as startup ∪ session — so the same mock was filtered through one
// accessor and leaked through the other. It assumes the agent and the workers share a PID namespace, which
// is the case for the normal `keploy mock <cmd>` wrap.

// scopedMockDb wraps the process-wide MockMemDb with a per-worker allowlist,
// narrowing the read views a scoped worker sees. Writers and consumers forward
// to the embedded db unchanged via interface embedding.
//
// The filter keeps a mock when it is in THIS worker's allowlist, OR when it
// belongs to no test at all (its name is absent from `universe`, the union of
// every test's mapped mock names). So a worker sees its own test's mocks plus
// genuinely-shared recordings (auth/handshake/bootstrap calls made outside any
// scope), but never another test's mocks. This is applied to BOTH the per-test
// and the session read tiers: `keploy mock record` classifies its captured
// calls into the session/config tier, so filtering only the per-test tier would
// leave every worker seeing the whole set (the isolation would be a no-op).
type scopedMockDb struct {
	integrations.MockMemDb
	// live: the view is resolved from the starts registry, per read, for pid.
	pid  uint32
	live bool
	// src (allow mode) makes the view follow its worker: every read resolves,
	// from the proxy's worker scopes as they are now, the nearest registered
	// worker the connection's process (chain) descends from, and that
	// worker's current test. A connection outlives a test — a keep-alive
	// client the worker reuses in its next test — so a view fixed when it
	// opened would serve the previous test's mocks. A wrap with neither live
	// nor src (a test seam for the forwarded capabilities) filters nothing.
	//
	// One request of a parser may read more than once (its per-test tier, its
	// session tier, a cursor peek); a test boundary landing between them can
	// mix two tests' views in that one request, as it would with any
	// boundary that lands mid-request.
	src   *Proxy
	chain []uint32 // the connection's process and its ancestors, nearest first
	born  uint64   // the proxy's scope sequence when this view was made (see Revision)
	// lastScope is the scope the latest cursor peek resolved; the commit after
	// it reuses it. A view serves one connection, whose requests are
	// sequential.
	lastScope string
}

// workerView is what a scoped call may see, resolved for one read.
type workerView struct {
	allow    map[string]struct{}
	universe map[string]struct{}
	scope    string // the worker's current scope ("" for none); keys stateful cursors
	// version is the proxy's scope sequence at the latest change to anything
	// this view is made from: the scope of a worker on its chain, or the
	// mapped universe. It only grows, and never returns to an earlier value.
	version uint64
}

// current resolves this view for one read.
func (s *scopedMockDb) current() workerView {
	if s.src == nil {
		return workerView{}
	}
	return s.src.workerViewFor(s.chain)
}

// visibleIn is whether a view shows m: in its allowlist, or belonging to no
// test. Without a filter every non-nil mock is visible.
func visibleIn(v workerView, m *models.Mock) bool {
	if m == nil {
		return false
	}
	if v.allow == nil || v.universe == nil {
		return true
	}
	if _, mine := v.allow[m.Name]; mine {
		return true
	}
	_, mapped := v.universe[m.Name]
	return !mapped // shared / unmapped mock — visible to everyone
}

// keep returns the mocks visible to this worker: those in its allowlist plus any
// that belong to no test (absent from the universe of mapped names), less what
// its open test consumed (DeleteFilteredMock). With no test open to the worker
// it is the whole pool less what ended tests consumed (spent).
//
// That holds for the calls a scoped view serves. A call served the bare
// manager (scopedFor: a connection opened before any worker registered, or
// one with no pid) sees the whole pool, held and spent recordings included,
// and its consume deletes them from the store.
func (s *scopedMockDb) keep(mocks []*models.Mock, err error) ([]*models.Mock, error) {
	if err != nil || s.passThrough() {
		return mocks, err
	}
	if s.live {
		return without(byStart(s.pid, mocks), s.consumed(workerView{})), nil
	}
	v := s.current()
	gone := s.consumed(v)
	if v.allow == nil || v.universe == nil {
		return without(mocks, gone), nil
	}
	out := mocks[:0:0]
	for _, m := range mocks {
		if visibleIn(v, m) && (gone == nil || !gone(m.Name)) {
			out = append(out, m)
		}
	}
	return out, nil
}

// consumed returns whether this view, resolved as v for one read, hides a
// recording, by name, as consumed: one the test open to it consumed
// (DeleteFilteredMock) or, with no test open, one an ended test consumed
// (spent). nil when it hides none.
func (s *scopedMockDb) consumed(v workerView) func(name string) bool {
	switch {
	case s.live:
		return s.usedUpIn(starts.Default.Scope(s.pid))
	case s.src == nil:
		return nil
	case v.allow == nil || v.universe == nil:
		if sl, ok := s.MockMemDb.(spentLister); ok {
			return sl.spentNames()
		}
		return nil
	}
	return s.usedUpIn(v.scope)
}

// without returns mocks less those whose name drop reports, mocks itself when
// drop is nil.
func without(mocks []*models.Mock, drop func(name string) bool) []*models.Mock {
	if drop == nil {
		return mocks
	}
	out := mocks[:0:0]
	for _, m := range mocks {
		if m != nil && drop(m.Name) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// usedUpHolder is the store's record of the recordings a worker's test
// consumed (MockManager.holdUsedUp), and spentLister its record of what ended
// tests consumed (MockManager.Spend).
type usedUpHolder interface {
	holdUsedUp(key string, kind models.Kind) bool
	usedUpKeys() func(key string) bool
}

type spentLister interface {
	spentNames() func(name string) bool
}

// usedUpIn returns whether a recording, by name, was consumed in scope: nil
// when none was.
func (s *scopedMockDb) usedUpIn(scope string) func(name string) bool {
	if scope == "" {
		return nil
	}
	h, ok := s.MockMemDb.(usedUpHolder)
	if !ok {
		return nil
	}
	held := h.usedUpKeys()
	if held == nil {
		return nil
	}
	prefix := keptPrefix(scope)
	return func(name string) bool { return held(prefix + name) }
}

// DeleteFilteredMock consumes mock. A recording of the test open to this
// worker (its mapping names it) is held used up for the test instead of
// deleted from the store: no later read of the test shows it, and once the
// test ends it is spent (Proxy.noteRetained), so no call between tests is
// served it, while a run of the same test again (a retry, -count=2, a watch
// mode rerun), a new scope, is served it afresh, as a test begun without a
// worker pid is (MockManager.SetScopeMocks). Any other mock is consumed by
// the store. A call served the bare manager instead of a scoped view (see
// keep) deletes even a scoped test's recording, as before.
func (s *scopedMockDb) DeleteFilteredMock(mock models.Mock) bool {
	h, ok := s.MockMemDb.(usedUpHolder)
	if !ok || s.passThrough() {
		return s.MockMemDb.DeleteFilteredMock(mock)
	}
	scope, own := s.openScopeOwns(&mock)
	if !own {
		return s.MockMemDb.DeleteFilteredMock(mock)
	}
	if !h.holdUsedUp(keptPrefix(scope)+mock.Name, mock.Kind) {
		return false // consumed already, as a delete of a deleted mock
	}
	s.MockMemDb.MarkMockAsUsed(mock)
	if !s.live && s.src != nil {
		s.src.noteRetained(scope, s.MockMemDb, mock)
	}
	return true
}

// openScopeOwns returns the scope of the test open to this worker and whether
// mock is that test's recording: its mapping names it.
func (s *scopedMockDb) openScopeOwns(mock *models.Mock) (string, bool) {
	switch {
	case s.live:
		scope := starts.Default.Scope(s.pid)
		owned, known := starts.Default.Owns(s.pid, mock.Name)
		return scope, scope != "" && known && owned
	case s.src != nil:
		v := s.current()
		if v.allow == nil || v.scope == "" {
			return "", false
		}
		_, mine := v.allow[mock.Name]
		return v.scope, mine
	}
	return "", false
}

// MarkMockAsUsed forwards a match that keeps the mock, and records a stateful
// dependency's per-test recording the worker's scope kept for its repeats,
// to be consumed when the scope ends (Proxy.noteRetained).
func (s *scopedMockDb) MarkMockAsUsed(mock models.Mock) bool {
	if !s.MockMemDb.MarkMockAsUsed(mock) {
		return false
	}
	if s.passThrough() {
		return true
	}
	// Only the open test's own: a recording every test may be served stays
	// for the next.
	if s.src != nil && !s.live && retainsForRepeats(&mock) && s.OwnedByOpenTest(&mock) {
		s.src.noteRetained(s.current().scope, s.MockMemDb, mock)
	}
	return true
}

// ExactServing forwards the store's exact serving
// (integrations.ExactServingReader). scopedFor hands a connection opened under
// exact serving the bare manager, but a view made before it began serves its
// connection for the connection's life, and under exact serving the parsers
// on it must see that MarkMockAsUsed's false for a named mock is a refusal.
func (s *scopedMockDb) ExactServing() bool { return integrations.ExactServing(s.MockMemDb) }

// FindStagedNotOwned forwards the store's lookup of a staged mock the open
// test does not own (integrations.StagedFinder), "" from a store that has
// none: under exact serving a Generic miss on a view made before it began is
// diagnosed as one on the bare manager is.
func (s *scopedMockDb) FindStagedNotOwned(kind models.Kind, match func(*models.Mock) bool) string {
	if f, ok := s.MockMemDb.(integrations.StagedFinder); ok {
		return f.FindStagedNotOwned(kind, match)
	}
	return ""
}

var _ integrations.StagedFinder = (*scopedMockDb)(nil)

// FindStagedNotOwnedByKeys forwards the store's
// (integrations.StagedKeyFinder), or, from one that has none, its
// FindStagedNotOwned.
func (s *scopedMockDb) FindStagedNotOwnedByKeys(kind models.Kind, keys []string, match func(*models.Mock) bool) string {
	if f, ok := s.MockMemDb.(integrations.StagedKeyFinder); ok {
		return f.FindStagedNotOwnedByKeys(kind, keys, match)
	}
	return s.FindStagedNotOwned(kind, match)
}

var _ integrations.StagedKeyFinder = (*scopedMockDb)(nil)

// StagedOwnerBegun forwards the store's (integrations.BegunOwnerReader);
// false from a store that cannot tell.
func (s *scopedMockDb) StagedOwnerBegun(name string) bool {
	if b, ok := s.MockMemDb.(integrations.BegunOwnerReader); ok {
		return b.StagedOwnerBegun(name)
	}
	return false
}

var _ integrations.BegunOwnerReader = (*scopedMockDb)(nil)

// passThrough reports whether this view passes every read and consume
// straight to the store it wraps: while the store serves each test exactly
// its own mocks (exact serving, the proven switch), it alone decides what
// every read shows and whether a consume may answer. A view made before
// exact serving began, which serves its connection for the connection's
// life, then narrows nothing by its worker's or start's scope, holds no
// consumed recording of its own, keys no cursor or kept recording by its
// scope, and hands each consume's answer, a refusal included, to the
// parser, as the bare manager scopedFor hands a connection opened under
// exact serving does. That holds while an agent serves one test process and
// one open test at a time, as it does under the switch (scope_proven); once
// workers run tests in parallel under it, the scoped cursors and kept
// recordings this bypasses are needed again.
func (s *scopedMockDb) passThrough() bool { return s.ExactServing() }

// ConsumedTestMocks forwards the store's answered test mocks
// (integrations.ExactConsumedReader), none when it keeps none, so a parser on
// a view made before exact serving began still tells a call its test made once
// more than it recorded from one it never recorded.
func (s *scopedMockDb) ConsumedTestMocks() []*models.Mock {
	if er, ok := s.MockMemDb.(integrations.ExactConsumedReader); ok {
		return er.ConsumedTestMocks()
	}
	return nil
}

// ServedInOpenTest forwards the store's account of what its open test was
// served (integrations.ExactServedReader), none when it keeps none, so a
// learn step on a scoped view reads the store's account as it would the bare
// manager's.
func (s *scopedMockDb) ServedInOpenTest() []*models.Mock {
	if er, ok := s.MockMemDb.(integrations.ExactServedReader); ok {
		return er.ServedInOpenTest()
	}
	return nil
}

var (
	_ integrations.ExactServingReader  = (*scopedMockDb)(nil)
	_ integrations.ExactConsumedReader = (*scopedMockDb)(nil)
	_ integrations.ExactServedReader   = (*scopedMockDb)(nil)
)

// ClaimantOf and ExactTakenCandidates forward what the store's open test's view says of the recordings it shares
// with the tests recorded beside it (integrations.ExactConcurrentReader),
// nothing from a store that says none: a parser's learn step on a view made
// before exact serving began reads them as it does on the bare manager.
func (s *scopedMockDb) ClaimantOf(name string) (string, bool) {
	if cr, ok := s.MockMemDb.(integrations.ExactConcurrentReader); ok {
		return cr.ClaimantOf(name)
	}
	return "", false
}

func (s *scopedMockDb) ExactTakenCandidates(kind models.Kind) []*models.Mock {
	if cr, ok := s.MockMemDb.(integrations.ExactConcurrentReader); ok {
		return cr.ExactTakenCandidates(kind)
	}
	return nil
}

var _ integrations.ExactConcurrentReader = (*scopedMockDb)(nil)

// StagedMocks forwards the store's staged mocks (integrations.StagedReader),
// none when it keeps none: the learn step's earlier places are the
// recording's, whatever this view's allowlist shows.
func (s *scopedMockDb) StagedMocks() []*models.Mock {
	if sr, ok := s.MockMemDb.(integrations.StagedReader); ok {
		return sr.StagedMocks()
	}
	return nil
}

var _ integrations.StagedReader = (*scopedMockDb)(nil)

// RangeRecordedBefore forwards the store's recordings before t
// (integrations.RecordedBeforeReader), whatever this view's allowlist shows:
// the learn step's earlier places are the recording's. Over a store that
// keeps no such reader it is an error, not what the view holds: the view
// cannot visit every recording made before t, and a caller that took what it
// holds for all of them would miss the ones kept on disk.
func (s *scopedMockDb) RangeRecordedBefore(t time.Time, visit func(*models.Mock) bool) error {
	if rb, ok := s.MockMemDb.(integrations.RecordedBeforeReader); ok {
		return rb.RangeRecordedBefore(t, visit)
	}
	return errNoRecordedBefore
}

// errNoRecordedBefore is a scoped view's answer over a store that cannot read
// every recording made before an instant.
var errNoRecordedBefore = errors.New("the mock store cannot read every recording made before an instant")

var _ integrations.RecordedBeforeReader = (*scopedMockDb)(nil)

// OwnedByTest implements integrations.TestOwnership: a mock is the open
// test's own when its worker's current allowlist (the test's mapping) names
// it.
func (s *scopedMockDb) OwnedByTest(m *models.Mock) (owned, known bool) {
	if s.live || m == nil {
		return false, false
	}
	v := s.current()
	if v.allow == nil {
		return false, false
	}
	_, owned = v.allow[m.Name]
	return owned, true
}

func (s *scopedMockDb) GetFilteredMocks() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetFilteredMocks())
}

func (s *scopedMockDb) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetFilteredMocksInWindow())
}

func (s *scopedMockDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetPerTestMocksInWindow())
}

func (s *scopedMockDb) GetSessionMocks() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetSessionMocks())
}

// GetSessionMocksInWindow forwards the window index of the wrapped store through
// the same name filter as GetSessionMocks; both filters are per mock, so their
// order does not matter. A store without the index is walked.
func (s *scopedMockDb) GetSessionMocksInWindow(start, end time.Time) ([]*models.Mock, error) {
	if r, ok := s.MockMemDb.(integrations.SessionWindowReader); ok {
		return s.keep(r.GetSessionMocksInWindow(start, end))
	}
	all, err := s.GetSessionMocks()
	if err != nil {
		return nil, err
	}
	out := make([]*models.Mock, 0, len(all))
	for _, mk := range all {
		if mk != nil && recordedIn(mk, start, end) {
			out = append(out, mk)
		}
	}
	return out, nil
}

// RangeSessionMocksWithKey forwards the key index of the wrapped store through
// the same name filter as GetSessionMocks. A store without the index is
// walked, which costs each lookup a whole GetSessionMocks snapshot; the agent
// always wraps the mock manager, which has it.
func (s *scopedMockDb) RangeSessionMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	if r, ok := s.MockMemDb.(integrations.SessionKeyReader); ok {
		if s.passThrough() {
			return r.RangeSessionMocksWithKey(ix, key, fn)
		}
		v := s.current()
		gone := s.consumed(v)
		return r.RangeSessionMocksWithKey(ix, key, func(mk *models.Mock) bool {
			return !visibleIn(v, mk) || (gone != nil && gone(mk.Name)) || fn(mk)
		})
	}
	all, err := s.GetSessionMocks()
	if err != nil {
		return err
	}
	for _, mk := range all {
		if mk != nil && slices.Contains(ix.Keys(mk), key) && !fn(mk) {
			break
		}
	}
	return nil
}

func (s *scopedMockDb) GetUnFilteredMocks() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetUnFilteredMocks())
}

func (s *scopedMockDb) GetSessionScopedMocks() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetSessionScopedMocks())
}

// The wrapper embeds the integrations.MockMemDb INTERFACE, so only that
// interface's method set is promoted. Everything a parser reaches for by type
// assertion — the revision counters and the by-kind readers — lives on
// *MockManager and is silently erased by the wrap, turning every kind-aware
// parser into its legacy branch for scoped workers only. Redis says so in its
// own fallback ("there is no legacy startup accessor"), and in `keploy mock
// replay` — the only mode where scoping exists — the startup tier is the whole
// pool, so that branch reads nothing.
//
// Forward them explicitly. The by-kind readers go through keep() for the same
// reason the plain ones do.
//
// The revision counters tell a parser that caches what it read (an index of
// the pool) when to read again. A view that follows its worker changes what it
// shows when the worker begins or ends a test, with no change to the pool, so
// its revisions add the view's version: the proxy's scope sequence at the
// latest change to this view (workerView.version). Both only grow, so the sum
// does; only this connection's parsers re-read, since another worker's test
// does not change this view's version; and a view that returns to showing
// what it showed before (between two tests) still reports a new revision.
//
// A view starts at the sequence value it took when it was made, above
// anything an earlier view reported: a parser that keys its cache on the
// store's address must not take a new view at a recycled address for the old
// one.

func (s *scopedMockDb) Revision() uint64 {
	var rev uint64
	if r, ok := s.MockMemDb.(interface{ Revision() uint64 }); ok {
		rev = r.Revision()
	}
	return rev + s.viewVersion()
}

func (s *scopedMockDb) RevisionByKind(kind models.Kind) uint64 {
	var rev uint64
	if r, ok := s.MockMemDb.(interface {
		RevisionByKind(models.Kind) uint64
	}); ok {
		rev = r.RevisionByKind(kind)
	}
	return rev + s.viewVersion()
}

func (s *scopedMockDb) viewVersion() uint64 {
	if s.src == nil {
		return 0
	}
	return max(s.born, s.current().version)
}

// A scoped call's stateful cursors are its scope's own: two workers reading
// the same request never advance each other, and a worker's next test (or a
// re-run of one) starts its sequences from the first recording instead of
// where the previous test left off. Dropping these on the wrap silently turned
// stateful replay off for every scoped worker: repeated requests replayed
// their first recording forever.
//
// The scope is the one this view's visibility uses, resolved on every read as
// visibility is — per peek, and the commit that follows it on this connection
// reuses it, so a test boundary landing between the two cannot advance another
// scope's cursor.
func (s *scopedMockDb) MockCursorIndex(key string, n int) int {
	c, ok := s.MockMemDb.(integrations.MockCursor)
	if !ok {
		return -1
	}
	switch {
	case s.passThrough():
		s.lastScope = ""
	case s.live:
		s.lastScope = starts.Default.Scope(s.pid)
	default:
		s.lastScope = s.current().scope
	}
	return c.MockCursorIndex(s.cursorKey(key), n)
}

func (s *scopedMockDb) AdvanceMockCursor(key string, servedIdx, n int) {
	if c, ok := s.MockMemDb.(integrations.MockCursor); ok {
		c.AdvanceMockCursor(s.cursorKey(key), servedIdx, n)
	}
}

// InTestWindow forwards integrations.TestWindows.
func (s *scopedMockDb) InTestWindow(t time.Time) bool {
	if w, ok := s.MockMemDb.(integrations.TestWindows); ok {
		return w.InTestWindow(t)
	}
	return false
}

// InRecordedTestWindow forwards integrations.TestWindows.
func (s *scopedMockDb) InRecordedTestWindow(t time.Time) (bool, bool) {
	if w, ok := s.MockMemDb.(integrations.TestWindows); ok {
		return w.InRecordedTestWindow(t)
	}
	return false, false
}

// InPool forwards integrations.PoolMembership: whose a mock is by the
// replay's scope table is the store's, whatever view a worker reads.
func (s *scopedMockDb) InPool(m *models.Mock) (bool, bool) {
	return integrations.InPool(s.MockMemDb, m)
}

// SameRecordedTestWindow forwards integrations.TestWindows.
func (s *scopedMockDb) SameRecordedTestWindow(a, b time.Time) (bool, bool) {
	if w, ok := s.MockMemDb.(integrations.TestWindows); ok {
		return w.SameRecordedTestWindow(a, b)
	}
	return false, false
}

// StatementCatalog forwards the staged set's statement catalog: describe-only,
// the same for every view (it serves no test's answer).
func (s *scopedMockDb) StatementCatalog(kind models.Kind) []*models.Mock {
	if c, ok := s.MockMemDb.(integrations.StatementCatalog); ok {
		return c.StatementCatalog(kind)
	}
	return nil
}

// Variables forwards the replay's variable table. Under the proven switch one
// test is open at a time in an agent, so every view shares the one table.
func (s *scopedMockDb) Variables() *variables.Table {
	if v, ok := s.MockMemDb.(integrations.VariableStore); ok {
		return v.Variables()
	}
	return nil
}

func (s *scopedMockDb) cursorKey(key string) string {
	if s.lastScope == "" {
		return key
	}
	return s.lastScope + "\x00" + key
}

// RecordedWindows, WindowChanged and StagingEpoch carry no mock data, so they
// pass straight through.
func (s *scopedMockDb) RecordedWindows() *models.WindowSchedule {
	if r, ok := s.MockMemDb.(integrations.RecordedWindowsReader); ok {
		return r.RecordedWindows()
	}
	return nil
}

// WindowChanged of a manager without the signal returns nil, a channel that
// never fires — the same "no signal" a consumer sees when the assertion fails.
func (s *scopedMockDb) WindowChanged() <-chan struct{} {
	if p, ok := s.MockMemDb.(integrations.WindowPacer); ok {
		return p.WindowChanged()
	}
	return nil
}

// The carry-over tier is a read tier like the others, so it goes through keep().
func (s *scopedMockDb) GetCarryOverMocks() ([]*models.Mock, error) {
	if r, ok := s.MockMemDb.(integrations.CarryOverReader); ok {
		return s.keep(r.GetCarryOverMocks())
	}
	return nil, nil
}

func (s *scopedMockDb) GetCarryOverMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	if r, ok := s.MockMemDb.(integrations.CarryOverReader); ok {
		return s.keep(r.GetCarryOverMocksByKind(kind))
	}
	return nil, nil
}

func (s *scopedMockDb) StagingEpoch() uint64 {
	if p, ok := s.MockMemDb.(integrations.WindowPacer); ok {
		return p.StagingEpoch()
	}
	return 0
}

func (s *scopedMockDb) GetFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	if bk, ok := s.MockMemDb.(interface {
		GetFilteredMocksByKind(models.Kind) ([]*models.Mock, error)
	}); ok {
		return s.keep(bk.GetFilteredMocksByKind(kind))
	}
	return s.keep(s.MockMemDb.GetFilteredMocks())
}

func (s *scopedMockDb) GetUnFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	if bk, ok := s.MockMemDb.(interface {
		GetUnFilteredMocksByKind(models.Kind) ([]*models.Mock, error)
	}); ok {
		return s.keep(bk.GetUnFilteredMocksByKind(kind))
	}
	return s.keep(s.MockMemDb.GetUnFilteredMocks())
}

func (s *scopedMockDb) GetStartupMocks() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetStartupMocks())
}

func (s *scopedMockDb) GetStartupMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	type byKind interface {
		GetStartupMocksByKind(models.Kind) ([]*models.Mock, error)
	}
	if bk, ok := s.MockMemDb.(byKind); ok {
		return s.keep(bk.GetStartupMocksByKind(kind))
	}
	return s.keep(s.MockMemDb.GetStartupMocks())
}

// SetWorkerScope registers (or replaces) the per-test mock allowlist for a
// worker PID. An empty/nil name list clears the worker's scope (it then serves
// the whole pool), matching "no mapping for this test ⇒ suite-level".
func (p *Proxy) SetWorkerScope(pid uint32, names []string) {
	if pid == 0 {
		return
	}
	if len(names) == 0 {
		p.ClearWorkerScope(pid)
		return
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	p.workerScopeMu.Lock()
	if p.workerScope == nil {
		p.workerScope = make(map[uint32]workerScopeEntry)
	}
	// The scope this one replaces ends here.
	ended := p.takeRetainedLocked(p.workerScope[pid].scope)
	// Each registration is a new scope (the worker's next test, or a re-run of
	// the same one), named by its place in the scope sequence, so its
	// stateful cursors start over. Replace (never mutate) the entry so a set
	// a read resolved stays an immutable snapshot.
	at := p.stampWorkerLocked(pid)
	p.workerScope[pid] = workerScopeEntry{allow: set, scope: fmt.Sprintf("w%d.%d", pid, at)}
	p.workerScopeMu.Unlock()
	consumeRetained(ended)
}

// retainedUse is a per-test recording of a stateful dependency a worker's
// scope was served and kept for the scope's repeats of the request (a cursor
// mock, models.ConsumeCursorSaturate), and the store that holds it.
type retainedUse struct {
	db   integrations.MockMemDb
	name string
}

// retainsForRepeats reports whether a match keeps m for its test's repeats
// rather than consuming it: a stated per-test HTTP recording of a stateful
// dependency (the HTTP integration's updateMock marks it used instead).
func retainsForRepeats(m *models.Mock) bool {
	return m.TestModeInfo.Lifetime == models.LifetimePerTest &&
		m.TestModeInfo.Consume == models.ConsumeCursorSaturate && m.StatesLifetime()
}

// noteRetained records that scope was served and kept mock (retainedUse).
func (p *Proxy) noteRetained(scope string, db integrations.MockMemDb, mock models.Mock) {
	if scope == "" {
		return
	}
	p.workerScopeMu.Lock()
	defer p.workerScopeMu.Unlock()
	if p.retained == nil {
		p.retained = make(map[string][]retainedUse)
	}
	p.retained[scope] = append(p.retained[scope], retainedUse{db: db, name: mock.Name})
}

// takeRetainedLocked removes and returns what scope kept for its repeats.
// workerScopeMu must be held.
func (p *Proxy) takeRetainedLocked(scope string) []retainedUse {
	if scope == "" {
		return nil
	}
	uses := p.retained[scope]
	delete(p.retained, scope)
	return uses
}

// consumeRetained spends what an ended scope kept for its repeats
// (MockManager.Spend): the test is over, so no later test, and no call
// between tests, is served its recordings, as when its matches consumed them;
// a later run of the same test is.
func consumeRetained(uses []retainedUse) {
	for _, u := range uses {
		if sp, ok := u.db.(interface{ Spend(...string) }); ok {
			sp.Spend(u.name)
		}
	}
}

// KeepForRepeats and KeptForRepeats key a worker's kept recordings by its
// scope, as its cursors are, so a later run of the same test starts afresh.
func (s *scopedMockDb) KeepForRepeats(name string) {
	if k, ok := s.MockMemDb.(integrations.RepeatKeeper); ok {
		k.KeepForRepeats(s.keptKey(name))
	}
}

func (s *scopedMockDb) KeptForRepeats() func(name string) bool {
	k, ok := s.MockMemDb.(integrations.RepeatKeeper)
	if !ok {
		return nil
	}
	kept := k.KeptForRepeats()
	if kept == nil {
		return nil
	}
	prefix := keptPrefix(s.scope())
	return func(name string) bool { return kept(prefix + name) }
}

func (s *scopedMockDb) keptKey(name string) string {
	return keptPrefix(s.scope()) + name
}

// scope is the scope this view serves a call in now: the test open to it, or
// its worker's.
func (s *scopedMockDb) scope() string {
	switch {
	case s.passThrough():
		return ""
	case s.live:
		return starts.Default.Scope(s.pid)
	case s.src != nil:
		return s.current().scope
	}
	return ""
}

func keptPrefix(scope string) string {
	if scope == "" {
		return ""
	}
	return scope + "\x00"
}

// OwnedByOpenTest reports whether m is the recording of the test open to this
// worker (integrations.TestOwner): its test's mapping names it. A mock mapped
// to no test, which every worker sees, is no test's own; so is every mock
// while no test is open to the worker. A view with no mapping to tell by asks
// the store it wraps.
func (s *scopedMockDb) OwnedByOpenTest(m *models.Mock) bool {
	if m == nil {
		return false
	}
	if s.passThrough() {
		return integrations.OwnedByOpenTest(s.MockMemDb, m)
	}
	if s.live {
		if owned, known := starts.Default.Owns(s.pid, m.Name); known {
			return owned
		}
		return integrations.OwnedByOpenTest(s.MockMemDb, m)
	}
	v := s.current()
	if v.allow == nil || v.universe == nil {
		return integrations.OwnedByOpenTest(s.MockMemDb, m)
	}
	_, mine := v.allow[m.Name]
	return mine
}

// Spent hides what an ended test kept for its repeats (MockManager.Spent)
// from this worker, unless it is the worker's current test's own: a later run
// of the same test is served its recordings again.
func (s *scopedMockDb) Spent(m *models.Mock) bool {
	sr, ok := s.MockMemDb.(integrations.SpentReader)
	if !ok || !sr.Spent(m) {
		return false
	}
	if s.live || s.src == nil || s.passThrough() {
		return true
	}
	_, mine := s.current().allow[m.Name]
	return !mine
}

// workerScopeEntry is a registered worker's current test: the mock names it
// may see, and its scope, which keys its stateful cursors.
type workerScopeEntry struct {
	allow map[string]struct{}
	scope string
}

// stampWorkerLocked records that pid's scope changed now, and returns the
// scope sequence value of the change. workerScopeMu must be held.
func (p *Proxy) stampWorkerLocked(pid uint32) uint64 {
	p.scopeSeq++
	if p.workerScopeAt == nil {
		p.workerScopeAt = make(map[uint32]uint64)
	}
	p.workerScopeAt[pid] = p.scopeSeq
	return p.scopeSeq
}

// ClearWorkerScope drops a worker's scope (called on /agent/scope/end and when a
// test has no mapping). Idempotent.
func (p *Proxy) ClearWorkerScope(pid uint32) {
	p.workerScopeMu.Lock()
	var ended []retainedUse
	if e, ok := p.workerScope[pid]; ok {
		ended = p.takeRetainedLocked(e.scope)
		delete(p.workerScope, pid)
		p.stampWorkerLocked(pid)
	}
	p.workerScopeMu.Unlock()
	consumeRetained(ended)
}

// ClearAllWorkerScopes wipes every worker scope and the mapped universe. Called
// when a replay session starts, so a worker of the session before that never
// sent /scope/end cannot leave an entry that mis-scopes a recycled PID.
func (p *Proxy) ClearAllWorkerScopes() {
	p.workerScopeMu.Lock()
	p.workerScope = nil
	p.retained = nil
	p.mappedUniverse = nil
	p.scopeSeq++
	p.universeAt = p.scopeSeq
	p.workerScopeMu.Unlock()
}

// SetMappedUniverse records the union of every test's mapped mock names (from
// mappings.yaml), so a scoped worker can tell "another test's mock" (drop) from
// a genuinely-shared, unmapped recording (keep). Pushed once when the replay CLI
// installs the scope table. Replaces (never mutates) the set.
func (p *Proxy) SetMappedUniverse(names []string) {
	var set map[string]struct{}
	if len(names) > 0 {
		set = make(map[string]struct{}, len(names))
		for _, n := range names {
			set[n] = struct{}{}
		}
	}
	p.workerScopeMu.Lock()
	p.mappedUniverse = set
	p.scopeSeq++
	p.universeAt = p.scopeSeq
	p.workerScopeMu.Unlock()
}

// scopedFor returns a mock view for an outgoing call from kpid. A call whose
// process descends from a registered worker gets a view that follows that
// worker from test to test, resolved on every read (see scopedMockDb.src);
// any other call gets the bare manager (whole pool), as does kpid == 0
// (non-eBPF platform / lookup miss).
//
// A connection opened before its process (or an ancestor) registered as a
// worker — a client a test binary dialled during its setup — keeps the bare
// manager: it is not wrapped on the chance that it might belong to a worker
// later, because a wrap is a new store identity, and parsers that key their
// indexes and stateful cursors on the store (enterprise Redis, Memcached,
// Couchbase, HBase) would then split one app's state per connection.
func (p *Proxy) scopedFor(kpid uint32, mgr integrations.MockMemDb) integrations.MockMemDb {
	if kpid == 0 || mgr == nil {
		return mgr
	}
	// Under exact serving (the proven switch) the manager itself decides what
	// every read shows (exact.go): no per-worker or per-start view narrows or
	// widens it.
	if mm, ok := mgr.(*MockManager); ok && mm.exactView() != nil {
		return mgr
	}
	if starts.Default.Active() {
		return &scopedMockDb{MockMemDb: mgr, pid: kpid, live: true}
	}
	p.workerScopeMu.RLock()
	nobody := len(p.workerScope) == 0
	p.workerScopeMu.RUnlock()
	if nobody {
		return mgr // fast path: no worker scoped — no /proc walk, exact old behavior
	}
	chain := ancestorChain(kpid)
	if p.workerViewFor(chain).allow == nil {
		return mgr
	}
	p.workerScopeMu.Lock()
	p.scopeSeq++
	born := p.scopeSeq
	p.workerScopeMu.Unlock()
	return &scopedMockDb{MockMemDb: mgr, src: p, chain: chain, born: born}
}

// WorkerViewForTest is the mock view a call from pid gets (scopedFor), for the
// tests of a parser outside this package that serve it as a scoped worker's
// call.
func (p *Proxy) WorkerViewForTest(pid uint32, mgr integrations.MockMemDb) integrations.MockMemDb {
	return p.scopedFor(pid, mgr)
}

// ancestorChain is pid and its ancestors, nearest first, read from /proc once
// per connection, outside any lock. Bounded so a reparent race or an
// unexpected /proc shape can never spin.
func ancestorChain(pid uint32) []uint32 {
	chain := make([]uint32, 0, 8)
	for i := 0; i < 32 && pid > 1; i++ {
		chain = append(chain, pid)
		ppid, ok := ppidFromStat(pid)
		if !ok {
			break
		}
		pid = ppid
	}
	return chain
}

// workerViewFor is what a call from a process with the given ancestor chain
// may see now: the allowlist and current scope of the nearest registered
// worker, or — when it descends from none — the whole pool.
func (p *Proxy) workerViewFor(chain []uint32) workerView {
	p.workerScopeMu.RLock()
	defer p.workerScopeMu.RUnlock()
	v := workerView{universe: p.mappedUniverse, version: p.universeAt}
	for _, pid := range chain {
		// Every worker on the chain counts, not only the nearest: one that
		// ended its test changed this view as much as one that began.
		v.version = max(v.version, p.workerScopeAt[pid])
		if e, ok := p.workerScope[pid]; ok && v.allow == nil {
			v.allow, v.scope = e.allow, e.scope
		}
	}
	return v
}

// ppidFromStat reads the parent PID of pid from /proc/<pid>/stat
// (utils.ReadProcStat).
func ppidFromStat(pid uint32) (uint32, bool) {
	st, ok := utils.ReadProcStat(int(pid))
	if !ok {
		return 0, false
	}
	return uint32(st.PPID), true
}

func byStart(pid uint32, mocks []*models.Mock) []*models.Mock {
	rank, universe, ok := starts.Default.View(pid, time.Now())
	if !ok {
		return mocks
	}
	type ranked struct {
		m *models.Mock
		r int
	}
	kept := make([]ranked, 0, len(mocks))
	for _, m := range mocks {
		if m == nil {
			continue
		}
		if r, ok := rank[m.Name]; ok {
			kept = append(kept, ranked{m, r})
			continue
		}
		if _, known := universe[m.Name]; !known {
			kept = append(kept, ranked{m, 9})
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].r < kept[j].r })
	out := make([]*models.Mock, len(kept))
	for i, k := range kept {
		out[i] = k.m
	}
	return out
}

// Every MockMemDb the proxy hands a parser must keep the stateful cursor.
var (
	_ integrations.MockCursor    = (*MockManager)(nil)
	_ integrations.MockCursor    = (*scopedMockDb)(nil)
	_ integrations.TestOwnership = (*scopedMockDb)(nil)
)

// StatesLifetimes answers for the recording the view is cut from: whether it
// states its kind mocks' lifetimes is the set's, not the worker's.
func (s *scopedMockDb) StatesLifetimes(kind models.Kind) bool {
	return integrations.StatesLifetimes(s.MockMemDb, kind)
}

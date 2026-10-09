package proxy

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
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
// that belong to no test (absent from the universe of mapped names).
func (s *scopedMockDb) keep(mocks []*models.Mock, err error) ([]*models.Mock, error) {
	if s.live && err == nil {
		return byStart(s.pid, mocks), nil
	}
	if err != nil {
		return mocks, err
	}
	v := s.current()
	if v.allow == nil || v.universe == nil {
		return mocks, nil
	}
	out := mocks[:0:0]
	for _, m := range mocks {
		if visibleIn(v, m) {
			out = append(out, m)
		}
	}
	return out, nil
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
		v := s.current()
		return r.RangeSessionMocksWithKey(ix, key, func(mk *models.Mock) bool {
			return !visibleIn(v, mk) || fn(mk)
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
	if s.live {
		s.lastScope = starts.Default.Scope(s.pid)
	} else {
		s.lastScope = s.current().scope
	}
	return c.MockCursorIndex(s.cursorKey(key), n)
}

func (s *scopedMockDb) AdvanceMockCursor(key string, servedIdx, n int) {
	if c, ok := s.MockMemDb.(integrations.MockCursor); ok {
		c.AdvanceMockCursor(s.cursorKey(key), servedIdx, n)
	}
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
	// Each registration is a new scope (the worker's next test, or a re-run of
	// the same one), named by its place in the scope sequence, so its
	// stateful cursors start over. Replace (never mutate) the entry so a set
	// a read resolved stays an immutable snapshot.
	at := p.stampWorkerLocked(pid)
	p.workerScope[pid] = workerScopeEntry{allow: set, scope: fmt.Sprintf("w%d.%d", pid, at)}
	p.workerScopeMu.Unlock()
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
	if _, ok := p.workerScope[pid]; ok {
		delete(p.workerScope, pid)
		p.stampWorkerLocked(pid)
	}
	p.workerScopeMu.Unlock()
}

// ClearAllWorkerScopes wipes every worker scope and the mapped universe. Called
// when a replay session starts, so a worker of the session before that never
// sent /scope/end cannot leave an entry that mis-scopes a recycled PID.
func (p *Proxy) ClearAllWorkerScopes() {
	p.workerScopeMu.Lock()
	p.workerScope = nil
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
	_ integrations.MockCursor = (*MockManager)(nil)
	_ integrations.MockCursor = (*scopedMockDb)(nil)
)

package proxy

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
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
// proxy (SetWorkerScope). An outgoing call's origin PID (OutgoingOptions.SrcPid,
// from the eBPF redirect map) is resolved up the /proc process tree to the
// nearest registered ancestor; that worker's per-test view is then intersected
// with its allowlist. Calls from unregistered process trees (and every call
// when no worker has scoped) fall through to the whole pool — so suite-level and
// single-worker sequential scoping behave exactly as before.
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
	allow    map[string]struct{} // mock names this worker's current test may see
	universe map[string]struct{} // union of ALL tests' mapped names (nil ⇒ no filtering)
	pid      uint32
	live     bool
}

// keep returns the mocks visible to this worker: those in its allowlist plus any
// that belong to no test (absent from the universe of mapped names).
func (s *scopedMockDb) keep(mocks []*models.Mock, err error) ([]*models.Mock, error) {
	if s.live && err == nil {
		return byStart(s.pid, mocks), nil
	}
	if err != nil || s.allow == nil || s.universe == nil {
		return mocks, err
	}
	out := mocks[:0:0]
	for _, m := range mocks {
		if s.visible(m) {
			out = append(out, m)
		}
	}
	return out, nil
}

// visible is keep's verdict on one mock. Without a filter every non-nil mock
// is visible.
func (s *scopedMockDb) visible(m *models.Mock) bool {
	if m == nil {
		return false
	}
	if s.allow == nil || s.universe == nil {
		return true
	}
	if _, mine := s.allow[m.Name]; mine {
		return true
	}
	_, mapped := s.universe[m.Name]
	return !mapped // shared / unmapped mock — visible to everyone
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
		return r.RangeSessionMocksWithKey(ix, key, func(mk *models.Mock) bool {
			return !s.visible(mk) || fn(mk)
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
// reason the plain ones do; the revision counters carry no mock data and pass
// straight through.

func (s *scopedMockDb) Revision() uint64 {
	if r, ok := s.MockMemDb.(interface{ Revision() uint64 }); ok {
		return r.Revision()
	}
	return 0
}

func (s *scopedMockDb) RevisionByKind(kind models.Kind) uint64 {
	if r, ok := s.MockMemDb.(interface {
		RevisionByKind(models.Kind) uint64
	}); ok {
		return r.RevisionByKind(kind)
	}
	return 0
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
		p.workerScope = make(map[uint32]map[string]struct{})
	}
	// Replace (never mutate) the inner map so a reference captured by scopedFor
	// under RLock stays an immutable snapshot.
	p.workerScope[pid] = set
	p.workerScopeMu.Unlock()
}

// ClearWorkerScope drops a worker's scope (called on /agent/scope/end and when a
// test has no mapping). Idempotent.
func (p *Proxy) ClearWorkerScope(pid uint32) {
	p.workerScopeMu.Lock()
	delete(p.workerScope, pid)
	p.workerScopeMu.Unlock()
}

// ClearAllWorkerScopes wipes every worker scope and the mapped universe. Called
// at replay-session teardown so a crashed worker that never sent /scope/end
// cannot leak an entry that later mis-scopes a recycled PID.
func (p *Proxy) ClearAllWorkerScopes() {
	p.workerScopeMu.Lock()
	p.workerScope = nil
	p.mappedUniverse = nil
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
	p.workerScopeMu.Unlock()
}

// scopedFor returns a mock view for an outgoing call from kpid. If some ancestor
// of kpid is a registered worker with an active scope, the returned view is
// narrowed to that worker's allowlist; otherwise the bare manager is returned
// (whole pool). kpid == 0 (non-eBPF platform / lookup miss) ⇒ whole pool.
func (p *Proxy) scopedFor(kpid uint32, mgr integrations.MockMemDb) integrations.MockMemDb {
	if kpid == 0 || mgr == nil {
		return mgr
	}
	if starts.Default.Active() {
		return &scopedMockDb{MockMemDb: mgr, pid: kpid, live: true}
	}
	p.workerScopeMu.RLock()
	if len(p.workerScope) == 0 {
		p.workerScopeMu.RUnlock()
		return mgr // fast path: nobody scoped — no /proc walk, exact old behavior
	}
	// Walk up the process tree to the nearest registered worker. Bounded so a
	// reparent race or an unexpected /proc shape can never spin.
	var allow map[string]struct{}
	pid := kpid
	for i := 0; i < 32 && pid > 1; i++ {
		if set, ok := p.workerScope[pid]; ok {
			allow = set
			break
		}
		ppid, ok := ppidFromStat(pid)
		if !ok {
			break
		}
		pid = ppid
	}
	universe := p.mappedUniverse
	p.workerScopeMu.RUnlock()

	if allow == nil {
		return mgr
	}
	return &scopedMockDb{MockMemDb: mgr, allow: allow, universe: universe}
}

// ppidFromStat reads the parent PID of pid from /proc/<pid>/stat. The comm field
// (2nd) is parenthesised and may itself contain ')' and spaces, so the state and
// ppid are read relative to the LAST ')': after it come " <state> <ppid> ...".
func ppidFromStat(pid uint32) (uint32, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(b)
	close := strings.LastIndexByte(s, ')')
	if close < 0 || close+2 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[close+2:]) // state, ppid, pgrp, ...
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(ppid), true
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

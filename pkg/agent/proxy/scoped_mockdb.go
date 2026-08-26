package proxy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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
// (DeleteFilteredMock) does not collide. Session/startup/connection mocks are
// shared and pass through unfiltered — a worker still sees shared auth/handshake
// recordings. It assumes the agent and the workers share a PID namespace, which
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
//
// The allowlist is resolved on EVERY read, not captured when the view is built.
// The view's lifetime is one TCP connection (scopedFor runs once per connection
// in handleConnection) while HTTP/1.1 keep-alive makes one connection carry
// requests from many tests, so a snapshot taken at connection time is stale for
// every test after the first one that connection serves.
type scopedMockDb struct {
	integrations.MockMemDb
	p         *Proxy
	workerPID uint32 // the registered worker this connection was resolved to
}

// keep returns the mocks visible to this worker: those in its allowlist plus any
// that belong to no test (absent from the universe of mapped names).
func (s *scopedMockDb) keep(mocks []*models.Mock, err error) ([]*models.Mock, error) {
	if err != nil {
		return mocks, err
	}
	s.p.workerScopeMu.RLock()
	allow := s.p.workerScope[s.workerPID]
	universe := s.p.mappedUniverse
	s.p.workerScopeMu.RUnlock()
	// No allowlist right now (worker between tests, or a test with no mapping)
	// ⇒ whole pool, matching "no mapping for this test ⇒ suite-level".
	if allow == nil || universe == nil {
		return mocks, nil
	}
	out := mocks[:0:0]
	for _, m := range mocks {
		if m == nil {
			continue
		}
		if _, mine := allow[m.Name]; mine {
			out = append(out, m)
			continue
		}
		if _, mapped := universe[m.Name]; !mapped {
			out = append(out, m) // shared / unmapped mock — visible to everyone
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

func (s *scopedMockDb) GetUnFilteredMocks() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetUnFilteredMocks())
}

func (s *scopedMockDb) GetSessionScopedMocks() ([]*models.Mock, error) {
	return s.keep(s.MockMemDb.GetSessionScopedMocks())
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

// RegisterRecordWorker registers a test worker's self-reported PID for
// RECORD-mode attribution. The replay-side registry (SetWorkerScope) cannot be
// reused: it carries a per-test allowlist that is installed and cleared at every
// test boundary, whereas record needs the worker to stay resolvable for the
// whole session — a keep-alive connection can emit its mock after /scope/end.
// Idempotent; cleared per record session in Record().
func (p *Proxy) RegisterRecordWorker(pid uint32) {
	if pid == 0 {
		return
	}
	p.workerScopeMu.Lock()
	if p.recordWorkers == nil {
		p.recordWorkers = make(map[uint32]struct{})
	}
	p.recordWorkers[pid] = struct{}{}
	p.workerScopeMu.Unlock()
}

// ClearRecordWorkers drops every registered record-mode worker. Called at the
// start of a record session so a PID from a previous run — possibly recycled
// onto an unrelated process by now — cannot mis-attribute this run's mocks.
func (p *Proxy) ClearRecordWorkers() {
	p.workerScopeMu.Lock()
	p.recordWorkers = nil
	p.workerScopeMu.Unlock()
}

// ResolveWorkerPID maps the kernel PID that opened an outgoing connection to the
// registered test worker that owns it — kpid itself, or its nearest registered
// /proc ancestor. Returns 0 when no worker has registered (fast path: no /proc
// read at all) or when none of kpid's ancestors is one.
//
// This is the record-side counterpart of scopedFor's walk, and it exists for the
// same reason: the process that opens the socket is often a CHILD of the worker
// (a browser's network-service process, a forked helper), so the kernel PID the
// eBPF redirect map reports never equals the PID the runner reported.
func (p *Proxy) ResolveWorkerPID(kpid uint32) uint32 {
	if kpid == 0 {
		return 0
	}
	p.workerScopeMu.RLock()
	registered := len(p.recordWorkers) > 0
	p.workerScopeMu.RUnlock()
	if !registered {
		return 0
	}
	return nearestRegistered(kpid, func(pid uint32) bool {
		p.workerScopeMu.RLock()
		_, ok := p.recordWorkers[pid]
		p.workerScopeMu.RUnlock()
		return ok
	})
}

// nearestRegistered walks pid up the /proc process tree and returns the first
// ancestor (pid itself included) that `registered` accepts, or 0 if the walk
// runs out of ancestors first. Bounded so a reparent race or an unexpected
// /proc shape can never spin.
func nearestRegistered(pid uint32, registered func(uint32) bool) uint32 {
	for i := 0; i < 32 && pid > 1; i++ {
		if registered(pid) {
			return pid
		}
		ppid, ok := ppidFromStat(pid)
		if !ok {
			return 0
		}
		pid = ppid
	}
	return 0
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
	p.workerScopeMu.RLock()
	scoped := len(p.workerScope) > 0
	p.workerScopeMu.RUnlock()
	if !scoped {
		return mgr // fast path: nobody scoped — no /proc walk, exact old behavior
	}
	worker := nearestRegistered(kpid, func(pid uint32) bool {
		p.workerScopeMu.RLock()
		_, ok := p.workerScope[pid]
		p.workerScopeMu.RUnlock()
		return ok
	})
	if worker == 0 {
		return mgr
	}
	// Only the worker identity is fixed for the connection's life — it cannot
	// change, the process that opened the socket keeps the same ancestry. The
	// ALLOWLIST is re-read per mock lookup; see scopedMockDb.
	return &scopedMockDb{MockMemDb: mgr, p: p, workerPID: worker}
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

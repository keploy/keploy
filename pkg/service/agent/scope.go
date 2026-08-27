package agent

import (
	"context"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The scope API lets a user's own test runner mark per-test boundaries so
// Keploy can attribute captured mocks to a test (record) and serve only that
// test's mocks (replay). A pytest fixture / go-test helper / jest hook / curl
// script calls, using the KEPLOY_MOCK_AGENT URL Keploy exports into the wrapped
// command:
//
//	POST {url}/agent/scope/begin  {"name":"<test>"}
//	POST {url}/agent/scope/end    {"name":"<test>"}
//
// Scoping is entirely optional: with no scope calls the set records/replays
// suite-level, which is still correct.

// scopeKey identifies an open record-mode scope by (reporting worker PID, test
// name). Keying by BOTH — not PID alone — keeps two things correct at once:
// parallel workers don't collide (distinct PIDs), and a single worker's nested
// or overlapping named scopes (or the legacy pid==0 path) don't clobber each
// other the way a PID-only key would.
type scopeKey struct {
	pid  uint32
	name string
}

// BeginScope opens a per-test scope. In record mode it stamps the begin time so
// captured mocks can later be bucketed to this test. In test mode it restricts
// the served pool to this test's mocks when the CLI supplied a mapping table.
//
// pid is the calling worker's PID (ScopeReq.Pid). When > 0 the scope is keyed to
// that worker so PARALLEL workers each get their own served view without
// stomping each other (Design A); pid == 0 falls back to the single global
// scope (sequential single-worker runs, and the suite-level default).
//
// attempt is the runner's attempt number (ScopeReq.Attempt): 0 for a first run,
// 1+ for a retry. On a retry this scope's own consumption is reset before the
// re-stage, so the retried test replays its tape from the start. Nothing else is
// reset — see resetConsumedForScope.
func (a *Agent) BeginScope(ctx context.Context, name string, pid, attempt int) (models.ScopeAck, error) {
	if name == "" {
		return models.ScopeAck{Reason: models.ScopeReasonEmptyName, Attempt: attempt}, nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.scopeMu.Lock()
		names, ok := a.scopeTable[name]
		tableSize := len(a.scopeTable)
		universe := a.mappedUniverse
		a.scopeMu.Unlock()
		// No per-test mapping for this test — leave the whole pool armed. All
		// three shapes below served the whole set silently before; they are
		// distinguished here because a runner needs to know WHICH it hit: an
		// absent mappings.yaml is a setup error, an absent NAME is a renamed
		// or never-recorded test.
		//
		// A retry (attempt > 0) that lands here has NOTHING to reset: the reset
		// is defined as "this scope's mock names", and none of these three cases
		// yields any. The ack carries attempt with retry_reset false so the
		// runner sees the request was understood and declined, rather than
		// broadening the reset to mocks that are not this scope's.
		switch {
		case tableSize == 0:
			a.logger.Debug("scope begin: NOT scoped, no per-test mapping table installed", zap.String("test", name), zap.Int("attempt", attempt))
			return models.ScopeAck{Reason: models.ScopeReasonNoMappingTable, Attempt: attempt}, nil
		case !ok:
			a.logger.Debug("scope begin: NOT scoped, this name is absent from the mapping table", zap.String("test", name), zap.Int("mapped_tests", tableSize), zap.Int("attempt", attempt))
			return models.ScopeAck{Reason: models.ScopeReasonUnmappedScope, Attempt: attempt}, nil
		case len(names) == 0:
			a.logger.Debug("scope begin: NOT scoped, this name is mapped to zero mocks", zap.String("test", name), zap.Int("attempt", attempt))
			return models.ScopeAck{Reason: models.ScopeReasonEmptyMapping, Attempt: attempt}, nil
		}
		if pid > 0 {
			// Parallel-safe: narrow ONLY this worker's served view, keyed by its
			// PID, so concurrent workers never overwrite one shared filter.
			//
			// A retry is NOT reset here. This path never re-stages the pool —
			// SetWorkerScope only narrows the view over a pool that consumption
			// mutates globally — so un-consuming the ledger would restore
			// nothing while reporting that it had. retry_reset stays false.
			if attempt > 0 {
				a.logger.Debug("scope begin: retry reset skipped, worker-scoped pools are not re-staged", zap.String("test", name), zap.Int("worker", pid), zap.Int("attempt", attempt))
			}
			a.logger.Debug("scope begin: worker-scoped pool", zap.String("test", name), zap.Int("worker", pid), zap.Int("mocks", len(names)))
			a.SetWorkerScope(uint32(pid), names)
			return models.ScopeAck{Scoped: true, Mocks: len(names), Reason: models.ScopeReasonWorkerScoped, Attempt: attempt}, nil
		}
		// No worker PID reported — restrict the single global pool (correct for
		// a sequential single-worker suite, the pre-Design-A behavior).
		ack := models.ScopeAck{Scoped: true, Mocks: len(names), Reason: models.ScopeReasonPoolRestricted, Attempt: attempt}
		if attempt > 0 {
			// BEFORE consumedSoFar, so the re-stage below sees the post-reset
			// ledger and re-arms exactly this test's mocks.
			ack.RestoredMocks, ack.RetryReset = a.resetConsumedForScope(ctx, name, names)
		}
		a.logger.Debug("scope begin: restricting served pool to test", zap.String("test", name), zap.Int("mocks", len(names)))
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			MockMapping: names,
			// Mocks belonging to no test stay reachable as overflow, after this
			// test's own. Without it a `beforeAll` recording is invisible for the
			// whole test and a call that needs it misses with candidates: 0,
			// while the per-worker path (SetWorkerScope above) already keeps such
			// mocks visible. The two paths agreed on nothing here until now.
			MockMappingUniverse: universe,
			UseMappingBased:     true,
			AfterTime:           models.BaseTime,
			BeforeTime:          time.Now(),
			// Subtract what this session already served. Upstream's persistent
			// ledger (#4534) exists but is not consulted unless the caller asks,
			// so without this every scope boundary re-stages a pristine pool and
			// resurrects the whole suite's consumption.
			AgentOwnsConsumed: true,
		}); err != nil {
			return models.ScopeAck{}, err
		}
		return ack, nil
	}

	// Record mode: mark the window start (agent clock), keyed by worker PID so
	// overlapping windows from parallel workers stay distinguishable. We do NOT
	// touch the syncMock ingress-correlation machinery here — that is driven by
	// incoming requests, which mock mode has none of.
	a.scopeMu.Lock()
	if a.workerOpen == nil {
		a.workerOpen = make(map[scopeKey]time.Time)
	}
	k := scopeKey{pid: uint32(pid), name: name}
	// A second begin for a scope that was never ended silently discards the
	// earlier window start, so every mock captured before this call is
	// attributed to no test and vanishes from mappings.yaml. The overwrite is
	// kept (changing it is a separate fix) but it is no longer silent.
	_, alreadyOpen := a.workerOpen[k]
	a.workerOpen[k] = time.Now()
	a.scopeMu.Unlock()
	a.logger.Debug("scope begin (record)", zap.String("test", name), zap.Int("worker", pid), zap.Bool("already_open", alreadyOpen), zap.Int("attempt", attempt))
	// attempt is echoed but changes nothing at record: recording has no served
	// pool to un-consume, and a retried test simply opens a second window under
	// the same name, which correlateScopes already handles.
	if alreadyOpen {
		return models.ScopeAck{Reason: models.ScopeReasonRecordAlreadyOpen, Attempt: attempt}, nil
	}
	return models.ScopeAck{Reason: models.ScopeReasonRecordWindowOpened, Attempt: attempt}, nil
}

// EndScope closes a per-test scope. In record mode it records the [begin, now]
// window (tagged with the worker PID) for later correlation. In test mode it
// restores this worker's whole-pool view so a call made between tests still
// matches.
func (a *Agent) EndScope(ctx context.Context, name string, pid int) error {
	if name == "" {
		return nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.scopeMu.Lock()
		_, scoped := a.scopeTable[name]
		a.scopeMu.Unlock()
		if !scoped {
			return nil
		}
		if pid > 0 {
			a.logger.Debug("scope end: clearing worker scope", zap.String("test", name), zap.Int("worker", pid))
			a.ClearWorkerScope(uint32(pid))
			return nil
		}
		a.logger.Debug("scope end: restoring whole pool", zap.String("test", name))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:  models.BaseTime,
			BeforeTime: time.Now(),
			// Same reason as BeginScope: this restore rebuilds the pool from the
			// pristine store, so without the flag it hands the next test every
			// mock this session has already served.
			AgentOwnsConsumed: true,
		})
	}

	a.scopeMu.Lock()
	k := scopeKey{pid: uint32(pid), name: name}
	start, ok := a.workerOpen[k]
	if ok {
		delete(a.workerOpen, k)
		a.scopeWindows = append(a.scopeWindows, models.ScopeWindow{Name: name, Start: start, End: time.Now(), PID: uint32(pid)})
	}
	a.scopeMu.Unlock()
	a.logger.Debug("scope end (record)", zap.String("test", name), zap.Int("worker", pid))
	return nil
}

// resolveOwner names the per-test scope that owns a capture made by worker `pid`
// at request time `ts`. This is the record-time half of the mock identity
// (owner, n): it runs while the scopes are still live, so a capture is stamped
// with its owner as it leaves the agent rather than being bucketed by timestamp
// after the fact.
//
// The rule is deliberately the SAME one the CLI's post-run correlateScopes uses
// (pkg/service/mock/record.go) so the two can be cross-checked against each
// other: the innermost -- latest-started -- window containing ts wins, and a
// window recorded by this exact worker is preferred over one from another
// worker, so overlapping parallel windows never steal each other's captures.
//
// The one thing it can see that correlateScopes cannot is a window that is
// still OPEN: a.workerOpen holds a start with no end yet, and is treated as
// extending to +infinity. That is the common case here -- the mock is usually
// emitted while its own test is still running.
//
// Returns "" when no window contains ts (a boot-time handshake before the first
// scope, a runner that declares no scopes at all, or a capture whose kind never
// stamps ReqTimestampMock). "" is the unowned namespace and keeps the legacy
// mock-N naming, so every un-scoped recording is unaffected.
func (a *Agent) resolveOwner(pid uint32, ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()

	// sameWorkerOnly restricts the scan to windows this worker opened. Scope
	// names are non-empty (BeginScope rejects ""), so "" is a safe sentinel.
	scan := func(sameWorkerOnly bool) string {
		best := ""
		var bestStart time.Time
		for _, w := range a.scopeWindows {
			if sameWorkerOnly && w.PID != pid {
				continue
			}
			if ts.Before(w.Start) || ts.After(w.End) {
				continue
			}
			if best == "" || w.Start.After(bestStart) {
				best, bestStart = w.Name, w.Start
			}
		}
		for k, start := range a.workerOpen {
			if sameWorkerOnly && k.pid != pid {
				continue
			}
			if ts.Before(start) {
				continue
			}
			if best == "" || start.After(bestStart) {
				best, bestStart = k.name, start
			}
		}
		return best
	}

	if pid != 0 {
		if owner := scan(true); owner != "" {
			return owner
		}
	}
	return scan(false)
}

// resetConsumedForScope un-consumes exactly the mocks that belong to `name`, so
// a retried test replays its own tape from the start. It returns how many
// entries the ledger actually dropped, and whether the reset ran at all.
//
// `names` is this scope's mappings.yaml entry, and that is what makes the reset
// SCOPE-PRECISE. The cumulative ledger is global, so clearing it wholesale would
// re-arm every mock the suite has consumed so far — the resurrection bug the
// ledger was introduced to fix. Passing only this scope's names means the retry
// gets its own tape back and no other test's.
//
// A proxy without the optional resetter reports false, so the acknowledgement
// says the reset did not happen rather than implying it did.
func (a *Agent) resetConsumedForScope(ctx context.Context, name string, names []string) (int, bool) {
	r, ok := a.Proxy.(coreAgent.ConsumedMockResetter)
	if !ok {
		a.logger.Debug("retry reset unavailable: this proxy cannot reset consumed mocks", zap.String("test", name))
		return 0, false
	}
	restored, err := r.ResetConsumedMocks(ctx, names)
	if err != nil {
		a.logger.Debug("failed to reset this scope's consumed mocks for a retry", zap.String("test", name), zap.Error(err))
		return 0, false
	}
	a.logger.Debug("scope begin: retry, re-arming this test's own mocks", zap.String("test", name), zap.Int("scope_mocks", len(names)), zap.Int("restored", restored))
	return restored, true
}

// GetScopeWindows returns the per-test windows collected this record session,
// consumed by the CLI to build mappings.yaml.
func (a *Agent) GetScopeWindows(_ context.Context) ([]models.ScopeWindow, error) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	out := make([]models.ScopeWindow, len(a.scopeWindows))
	copy(out, a.scopeWindows)
	return out, nil
}

// SetScopeTable installs the replay-time per-test name→mock-names table the CLI
// read from mappings.yaml.
func (a *Agent) SetScopeTable(_ context.Context, table map[string][]string) error {
	a.scopeMu.Lock()
	a.scopeTable = table
	a.scopeMu.Unlock()

	// Push the union of every test's mapped mock names to the proxy so a scoped
	// worker can tell another test's mock (hide) from a genuinely-shared,
	// unmapped recording (keep). De-duplicated across tests.
	seen := make(map[string]struct{})
	universe := make([]string, 0)
	for _, names := range table {
		for _, n := range names {
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				universe = append(universe, n)
			}
		}
	}
	a.SetMappedUniverse(universe)
	// Cached for BeginScope's pid == 0 branch, which needs the same set to tell
	// "another test's mock" from "belongs to no test" when it narrows the pool.
	a.scopeMu.Lock()
	a.mappedUniverse = universe
	a.scopeMu.Unlock()
	return nil
}

// DrainCapturedMocks returns and clears the mocks captured on miss during a
// `--on-miss record` replay session, so the CLI can append them to the set.
func (a *Agent) DrainCapturedMocks(_ context.Context) ([]*models.Mock, error) {
	return httpparser.DrainCaptured(), nil
}

// MockStats returns a non-draining snapshot for /agent/mock/stats. Consumed and
// missed totals are surfaced in the CLI's end-of-run summary (they drain their
// capture windows), so this live endpoint reports the loaded count only.
func (a *Agent) MockStats(_ context.Context) (models.MockStats, error) {
	a.scopeMu.Lock()
	loaded := a.loadedMocks
	a.scopeMu.Unlock()
	return models.MockStats{Loaded: loaded}, nil
}

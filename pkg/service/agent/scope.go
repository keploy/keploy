package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	if pid > 0 {
		a.pidNsWarnOnce.Do(func() { a.warnIfWorkerPIDUnresolvable(pid) })
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.scopeMu.Lock()
		names, ok := a.scopeTable[name]
		tableSize := len(a.scopeTable)
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
		consumed := a.consumedSoFar(ctx)
		a.logger.Debug("scope begin: restricting served pool to test", zap.String("test", name), zap.Int("mocks", len(names)), zap.Int("already_consumed", len(consumed)))
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			MockMapping:        names,
			UseMappingBased:    true,
			AfterTime:          models.BaseTime,
			BeforeTime:         time.Now(),
			TotalConsumedMocks: consumed,
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
	// Register the worker with the proxy so a call made by this worker OR ANY OF
	// ITS CHILDREN (a browser's network process, a forked helper) is attributed
	// to it at capture time. Correlation happens after the runner has exited, so
	// this is the last moment the process tree still exists to be walked.
	if wr, ok := a.Proxy.(coreAgent.WorkerRegistrar); ok && pid > 0 {
		wr.RegisterRecordWorker(uint32(pid))
	}
	a.logger.Debug("scope begin (record)", zap.String("test", name), zap.Int("worker", pid), zap.Bool("already_open", alreadyOpen), zap.Int("attempt", attempt))
	// attempt is echoed but changes nothing at record: recording has no served
	// pool to un-consume, and a retried test simply opens a second window under
	// the same name, which correlateScopes already handles.
	if alreadyOpen {
		return models.ScopeAck{Reason: models.ScopeReasonRecordAlreadyOpen, Attempt: attempt}, nil
	}
	return models.ScopeAck{Reason: models.ScopeReasonRecordWindowOpened, Attempt: attempt}, nil
}

// warnIfWorkerPIDUnresolvable fires once per agent process when the PID a test
// runner reported does not exist in the AGENT's PID namespace. Every per-PID
// mechanism — record-time attribution (Proxy.ResolveWorkerPID) and replay-time
// worker scoping (Proxy.scopedFor) — resolves the kernel PID of an outgoing
// connection up /proc looking for this number. If the number is from another
// namespace, no ancestor can ever match, both silently degrade to whole-set
// behaviour, and the run LOOKS isolated while it is not.
//
// One os.Stat per session; never on a traffic path. A false negative is
// possible (an unrelated process may hold that PID number in the agent's
// namespace) — the end-of-record attribution warning covers that case by
// outcome instead of by namespace.
func (a *Agent) warnIfWorkerPIDUnresolvable(pid int) {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
		return
	}
	a.logger.Warn("per-test mock scoping is degraded: the worker PID reported by your test runner does not exist in the agent's PID namespace, so no outgoing call can be attributed to a worker",
		zap.Int("reported_worker_pid", pid),
		zap.String("next_step", "run the agent in the same PID namespace as the test runner (e.g. docker run --pid=host), or run the runner inside the agent's container; until then mocks are attributed by timestamp and replay serves the whole set"))
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
		// The whole pool is restored MINUS what has already been served. Without
		// the consumed set this call is the other half of the resurrection bug:
		// a test with no mapping entry (renamed, or it recorded nothing) reads
		// the pool exactly as EndScope left it, so every mock the suite has
		// consumed so far becomes matchable again.
		consumed := a.consumedSoFar(ctx)
		a.logger.Debug("scope end: restoring whole pool", zap.String("test", name), zap.Int("already_consumed", len(consumed)))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:          models.BaseTime,
			BeforeTime:         time.Now(),
			TotalConsumedMocks: consumed,
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

// consumedSoFar returns the cumulative set of mocks already served this
// session, for UpdateMockParams' filterOutDeleted gate.
//
// Every scope re-stage rebuilds the served pool from the PRISTINE store
// (loadPerTestMocks over storage.filtered / storage.diskMocks), so without
// this the re-stage undoes every consumption that came before it — the same
// mock is served twice and a later one never at all. filterOutDeleted is
// gated on a non-nil map, so a nil return here is "unknown, filter nothing",
// which is exactly the old behaviour.
//
// Deliberately NOT GetConsumedMocks: that one drains, and the CLI's
// end-of-run `mock replay summary` counts what the drain returns.
func (a *Agent) consumedSoFar(ctx context.Context) map[string]models.MockState {
	r, ok := a.Proxy.(coreAgent.ConsumedMockTotalsReader)
	if !ok {
		return nil
	}
	consumed, err := r.TotalConsumedMocks(ctx)
	if err != nil {
		a.logger.Debug("failed to read cumulative consumed mocks; the scope re-stage will not filter them", zap.Error(err))
		return nil
	}
	return consumed
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

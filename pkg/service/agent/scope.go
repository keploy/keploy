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
func (a *Agent) BeginScope(ctx context.Context, name string, pid int) error {
	if name == "" {
		return nil
	}
	if pid > 0 {
		a.pidNsWarnOnce.Do(func() { a.warnIfWorkerPIDUnresolvable(pid) })
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.scopeMu.Lock()
		names, ok := a.scopeTable[name]
		a.scopeMu.Unlock()
		if !ok || len(names) == 0 {
			// No per-test mapping for this test — leave the whole pool armed.
			return nil
		}
		if pid > 0 {
			// Parallel-safe: narrow ONLY this worker's served view, keyed by its
			// PID, so concurrent workers never overwrite one shared filter.
			a.logger.Debug("scope begin: worker-scoped pool", zap.String("test", name), zap.Int("worker", pid), zap.Int("mocks", len(names)))
			a.SetWorkerScope(uint32(pid), names)
			return nil
		}
		// No worker PID reported — restrict the single global pool (correct for
		// a sequential single-worker suite, the pre-Design-A behavior).
		a.logger.Debug("scope begin: restricting served pool to test", zap.String("test", name), zap.Int("mocks", len(names)))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			MockMapping:     names,
			UseMappingBased: true,
			AfterTime:       models.BaseTime,
			BeforeTime:      time.Now(),
		})
	}

	// Record mode: mark the window start (agent clock), keyed by worker PID so
	// overlapping windows from parallel workers stay distinguishable. We do NOT
	// touch the syncMock ingress-correlation machinery here — that is driven by
	// incoming requests, which mock mode has none of.
	a.scopeMu.Lock()
	if a.workerOpen == nil {
		a.workerOpen = make(map[scopeKey]time.Time)
	}
	a.workerOpen[scopeKey{pid: uint32(pid), name: name}] = time.Now()
	a.scopeMu.Unlock()
	// Register the worker with the proxy so a call made by this worker OR ANY OF
	// ITS CHILDREN (a browser's network process, a forked helper) is attributed
	// to it at capture time. Correlation happens after the runner has exited, so
	// this is the last moment the process tree still exists to be walked.
	if wr, ok := a.Proxy.(coreAgent.WorkerRegistrar); ok && pid > 0 {
		wr.RegisterRecordWorker(uint32(pid))
	}
	a.logger.Debug("scope begin (record)", zap.String("test", name), zap.Int("worker", pid))
	return nil
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
		a.logger.Debug("scope end: restoring whole pool", zap.String("test", name))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:  models.BaseTime,
			BeforeTime: time.Now(),
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

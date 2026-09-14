package mock

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	rec "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// mockDrainGrace bounds how long Record waits for the agent to hand over the
// last few mocks after the wrapped runner has exited.
const mockDrainGrace = 5 * time.Second

// mockDrainQuiet is how long the outgoing stream must stay silent before the
// drain concludes the agent has handed over everything.
//
// The window it covers is small but real: the agent finishes parsing the last
// dependency response, emits the mock, and it travels agent -> gob stream ->
// CLI, while the CLI independently observes the wrapped runner exit. Measured
// on a failing reproduction that gap ran to a median of 6.5ms and a maximum of
// 22.8ms. 500ms is an order of magnitude of headroom on that, is paid once per
// recording, and is bounded by mockDrainGrace above.
const mockDrainQuiet = 500 * time.Millisecond

// Record runs the wrapped test command and captures every outgoing dependency
// call into the configured named mock set. It writes ONLY mocks (no incoming
// test cases), overwrites the set in place so a re-record produces a clean
// diff, correlates any per-test scopes the runner reported into mappings.yaml,
// pushes the set to the store, and propagates the runner's exit code.
func (m *mockService) Record(ctx context.Context) error {
	name := m.setName()
	m.logger.Info("Recording mocks for your test command",
		zap.String("mock-set", name),
		zap.String("command", m.config.Command))

	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)
	ctx, cancel := context.WithCancel(ctx)
	// Writes must outlive a SIGINT teardown so the tail of the recording still
	// reaches disk (same rationale as pkg/service/record's persistCtx).
	persistCtx := context.WithoutCancel(ctx)

	var stopReason string
	defer func() {
		m.notifyShutdown()
		// Cancel the errgroup ctx so the agent-monitor / app-runner goroutines
		// (bound to this ctx by Setup/Run) observe cancellation and unwind,
		// otherwise the drain below waits its full 30s budget every run.
		cancel()
		if err := utils.DrainErrGroup(m.logger, "mock-record", errGrp, 30*time.Second); err != nil {
			utils.LogError(m.logger, err, "failed to drain mock-record goroutines")
		}
	}()

	// 1. Instrument: start the agent, hooks and proxy in mock mode (no ingress
	//    port relocation — the runner is not a server).
	// --pass-through-ports lands in BypassRules (config.SetByPassPorts, driven by
	// cli/provider/cmd.go). The proxy-level rules were already forwarded via
	// OutgoingOptions.Rules below, but the KERNEL-level bypass is a separate
	// channel and mock mode never populated it -- so a bypassed port was still
	// pulled through the proxy and recorded. Integration record/test have always
	// set this (service/record/record.go, service/replay/replay.go); mock mode
	// was the only path that did not.
	passPortsUint := config.GetByPassPorts(m.config)

	if err := m.instrumentation.Setup(ctx, m.config.Command, models.SetupOptions{
		Container:        m.config.ContainerName,
		CommandType:      m.config.CommandType,
		DockerDelay:      m.config.BuildDelay,
		BuildDelay:       m.config.BuildDelay,
		Mode:             models.MODE_RECORD,
		MockMode:         true,
		ConfigPath:       m.config.ConfigPath,
		PassThroughPorts: passPortsUint,
	}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	// 2. Overwrite the named set. A full re-record drops the previous mocks so
	//    the result is a clean rewrite; --partial keeps them and replaces only
	//    the owners this run actually captures.
	//
	//    The full path MUST keep wiping: an owner deleted from the suite would
	//    otherwise keep its recording forever, and the next replay would serve
	//    a test that no longer exists.
	m.mockDB.SetPartialRecord(m.config.Mock.Partial)
	if m.config.Mock.Partial {
		m.logger.Info("partial re-record: keeping the existing set and replacing only the tests this run captures",
			zap.String("mock-set", name))
	} else {
		if err := m.mockDB.DeleteMocksForSet(persistCtx, name); err != nil {
			m.logger.Debug("no existing mock set to overwrite (or delete failed)", zap.String("mock-set", name), zap.Error(err))
		}
		// The mapping file is NOT removed by DeleteMocksForSet: it survives on disk
		// and ResetCounterID below reissues the same mock-N names, so a surviving
		// mapping would attribute this run's mocks to the previous run's tests.
		if m.mappingDB != nil {
			if err := m.mappingDB.Delete(persistCtx, name); err != nil {
				m.logger.Warn("failed to clear the previous per-test mock mappings; stale test entries may survive this re-record", zap.String("mock-set", name), zap.Error(err))
			}
		}
	}
	m.mockDB.ResetCounterID()

	// 3. Arm the record proxy and stream captured mocks.
	captureCtx, stopCapture := context.WithCancel(context.WithoutCancel(ctx))
	defer stopCapture()
	outgoing, err := m.instrumentation.GetOutgoing(captureCtx, models.OutgoingOptions{
		Rules:                     m.config.BypassRules,
		MongoPassword:             m.config.Test.MongoPassword,
		MysqlPorts:                m.config.MysqlPorts,
		DisableMysqlAutoDetect:    m.config.DisableMysqlAutoDetect,
		DisableMysqlEndpointDrift: m.config.DisableMysqlEndpointDrift,
		PassThroughPorts:          m.config.Record.PassThroughPorts,
		PassThroughHosts:          m.config.Record.PassThroughHosts,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		stopReason = "failed to start capturing outgoing calls"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	// recorded remembers each captured mock's name + request timestamp so the
	// scope-window correlation after the run can build mappings.yaml.
	var recorded []capturedMock
	mockCount := 0
	// Bumped for every mock that arrives, whether or not it ends up persisted,
	// and read by the drain below while the consumer is still running - hence
	// atomic. mockCount stays a plain int: it is only read after consumerDone.
	var mocksSeen atomic.Int64
	consumerDone := make(chan struct{})
	go func() {
		defer utils.Recover(m.logger)
		defer close(consumerDone)
		for mk := range outgoing {
			mocksSeen.Add(1)
			mctx := &rec.MockContext{Mock: mk, TestSetID: name}
			if err := m.hooks.BeforeMockInsert(ctx, mctx); err != nil {
				m.logger.Debug("BeforeMockInsert hook failed", zap.Error(err), zap.String("mock", mk.Name))
			}
			if mctx.Skip {
				continue
			}
			if err := m.mockDB.InsertMock(persistCtx, mk, name); err != nil {
				if errors.Is(err, models.ErrMockEncode) {
					m.logger.Warn("dropping one unencodable mock and continuing", zap.String("kind", mk.GetKind()), zap.Error(err))
					continue
				}
				utils.LogError(m.logger, err, "failed to persist mock", zap.String("mock", mk.Name))
				continue
			}
			if err := m.hooks.AfterMockInsert(ctx, &rec.MockContext{Mock: mk, TestSetID: name}); err != nil {
				m.logger.Debug("AfterMockInsert hook failed", zap.Error(err), zap.String("mock", mk.Name))
			}
			mockCount++
			recorded = append(recorded, capturedMock{name: mk.Name, ts: mk.Spec.ReqTimestampMock, pid: mk.SourcePID, owner: mk.Owner})
		}
	}()

	// 4. Optional record timer.
	if m.config.Mock.RecordTimer > 0 {
		errGrp.Go(func() error {
			m.logger.Info("recording will stop after " + m.config.Mock.RecordTimer.String())
			select {
			case <-time.After(m.config.Mock.RecordTimer):
				_ = utils.Stop(m.logger, "record timer elapsed")
			case <-ctx.Done():
			}
			return nil
		})
	}

	// 5. Run the wrapped runner and block until it exits. Its exit — clean or
	//    failing — is the NORMAL end of a mock recording, not an app crash.
	appErr := m.instrumentation.Run(ctx, models.RunOptions{AppCommand: m.config.Command})

	// 6. Drain the trailing mocks, THEN stop capturing.
	//
	// The order is the entire point. stopCapture() cancels the context the
	// outgoing stream is built on (http.NewRequestWithContext in
	// pkg/platform/http/agent.go), so cancelling first aborts the request, the
	// decoder errors, the mock channel closes, and consumerDone fires in tens
	// of microseconds. A grace period applied after that waits on an
	// already-closed channel and does nothing at all - which is how a mock the
	// agent had already written to the wire was still lost, silently, with the
	// agent-side accounting reporting success.
	//
	// So wait for the stream to fall quiet before tearing it down. Quiescence
	// rather than a fixed sleep: a sleep long enough to be safe would be paid
	// in full by every recording, and one short enough not to hurt would still
	// be a race. This returns as soon as the agent stops sending.
	m.drainTrailingMocks(ctx, consumerDone, &mocksSeen)
	stopCapture()
	select {
	case <-consumerDone:
	case <-time.After(mockDrainGrace):
		m.logger.Debug("timed out waiting for the mock consumer to finish after teardown")
	}

	if ctx.Err() != nil { // user Ctrl+C
		m.logger.Info("recording stopped", zap.Int("mocks", mockCount), zap.String("mock-set", name))
		return nil
	}

	// 7. Correlate per-test scope windows into mappings.yaml (best-effort).
	if m.mappingDB != nil {
		if reader, ok := m.instrumentation.(ScopeReader); ok {
			windows, werr := reader.GetScopeWindows(persistCtx)
			if werr != nil {
				m.logger.Debug("failed to read per-test scope windows; recording suite-level", zap.Error(werr))
			} else if len(windows) > 0 {
				byTest := mapOwners(m.logger, windows, recorded)
				if len(byTest) > 0 {
					if err := m.mappingDB.UpsertBatchReplacing(persistCtx, name, byTest); err != nil {
						m.logger.Warn("failed to write per-test mappings; replay will serve the whole set per test", zap.Error(err))
					} else {
						m.logger.Info("wrote per-test mock mappings", zap.Int("tests", len(byTest)), zap.String("mock-set", name))
					}
				}
			}
		}
	}

	// 8. Publish the set to the store (registry upload in enterprise; no-op on files).
	if err := m.store.Push(persistCtx, name); err != nil {
		m.logger.Warn("failed to publish mock set to the store", zap.String("mock-set", name), zap.Error(err))
	}

	// Report how much of this recording belongs to NO test.
	//
	// This is the only signal that catches a runner whose per-test scoping has
	// silently stopped working. A Playwright fixture covers hook traffic through
	// an undocumented fixture mode; if that mode is ever renamed, the fixture
	// keeps running, every test still passes, and the whole of `beforeAll` --
	// login, page load, the bulk of the traffic -- lands here unowned instead.
	// Unowned mocks are served to every test as shared overflow, so nothing
	// downstream fails and the suite stays green while per-test isolation is
	// gone. Probing the runner cannot see that; counting the result can.
	//
	// A non-zero count is NOT itself a fault: a runner that reports no scopes at
	// all records everything this way, by design. It is the RATIO that matters
	// on a suite that does report scopes.
	unowned := 0
	for _, mk := range recorded {
		if mk.owner == "" {
			unowned++
		}
	}
	m.logger.Info("recorded mocks",
		zap.Int("mocks", mockCount),
		zap.Int("unowned", unowned),
		zap.String("mock-set", name))
	if unowned > 0 && unowned < mockCount {
		m.logger.Warn("some captures belong to no test and will be served to EVERY test as shared overflow",
			zap.Int("unowned", unowned),
			zap.Int("mocks", mockCount),
			zap.String("next_step", "expected for traffic outside any test (app startup). If it covers hook traffic the runner meant to scope, per-test isolation has degraded -- check that the runner still opens a scope around beforeAll/afterAll"))
	}
	if mockCount == 0 {
		m.logger.Warn("no outgoing calls were captured; the runner made no mockable dependency calls, or its traffic was not intercepted",
			zap.String("next_step", "confirm the test command actually calls an external dependency (HTTP, MySQL, ...), and on macOS run it via a docker command"))
	}

	// 9. Propagate the runner's exit code so a CI 're-record on merge' job fails
	//    when the tests fail.
	m.propagateExit(appErr, "record")
	return nil
}

// capturedMock is one recorded mock's name + request timestamp + source worker
// PID, used to correlate mocks into per-test scope windows.
type capturedMock struct {
	name string
	ts   time.Time
	pid  uint32 // source worker PID (0 if unknown); enables exact parallel attribution
	// owner is the scope the AGENT stamped on this capture as it was emitted
	// (models.Mock.Owner), resolved against the scope map while that scope was
	// still live. Empty when no scope contained the call, or when the agent
	// predates the stamp -- both fall back to the timestamp correlation below.
	owner string
}

// correlateScopes buckets each recorded mock into the per-test scope window its
// request timestamp falls within, producing the mappings.yaml structure. A mock
// that matches no window — a boot-time handshake before the first scope, or a
// Playwright `beforeAll`, whose fixtures are per-test so it runs before any
// scope/begin — is left out of every test's mapping.
//
// Such a mock is NOT lost. At replay it becomes the shared overflow tier: the
// agent hands MockMappingUniverse alongside the per-test mapping, and
// pkg.FilterTcsMocksMappingWithShared keeps every name absent from that union
// visible to each scoped test, after that test's own mocks. The per-worker path
// reaches the same outcome by a different route (proxy.scopedMockDb.keep).
//
// This used to read "it stays reusable/session-tier at replay", which was false
// for HTTP: the recorder tags HTTP captures "HTTP_CLIENT", DeriveLifetime
// classifies those LifetimePerTest (HTTP is excluded from the lax promotion in
// models.kindsWithLaxTaggedSessionPromotion), so they never reach the session
// pool that survives mapping-based narrowing. The mock was silently invisible
// for the whole of every scoped test and a call needing it missed with
// candidates: 0.
//
// When a mock carries a source PID it is attributed to the SAME worker's window
// (exact, so overlapping parallel windows don't steal each other's mocks). That
// PID match is exact only when the worker made the call itself and the agent
// shares its PID namespace (the normal `keploy mock <cmd>` wrap); a call made by
// a CHILD of the worker, or a containerized/cross-namespace worker whose
// self-reported PID differs from the kernel PID, falls back to the timestamp
// scan — which is exact for sequential record and best-effort under overlap.
func correlateScopes(windows []models.ScopeWindow, mocks []capturedMock) map[string][]models.MockEntry {
	sortScopeWindows(windows)
	byTest := make(map[string][]models.MockEntry)
	for _, mk := range mocks {
		if name := scopeNameFor(windows, mk); name != "" {
			byTest[name] = append(byTest[name], models.MockEntry{Name: mk.name})
		}
	}
	return byTest
}

// sortScopeWindows orders windows by start so overlapping scopes resolve to the
// innermost (latest-started) one deterministically. scopeNameFor requires it.
func sortScopeWindows(windows []models.ScopeWindow) {
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].Start.Before(windows[j].Start) })
}

// scopeNameFor returns the test that owns mk by timestamp, or "" for none.
// windows must already be sorted (sortScopeWindows).
func scopeNameFor(windows []models.ScopeWindow, mk capturedMock) string {
	// containingWindow returns the innermost (latest-started) window that contains
	// the mock's timestamp. When sameWorkerOnly is set (the mock carries a PID),
	// only windows recorded by that exact worker are considered — so overlapping
	// windows from OTHER parallel workers never steal the mock.
	containingWindow := func(sameWorkerOnly bool) int {
		best := -1
		for i, w := range windows {
			if sameWorkerOnly && w.PID != mk.pid {
				continue
			}
			if mk.ts.Before(w.Start) || mk.ts.After(w.End) {
				continue
			}
			if best == -1 || windows[i].Start.After(windows[best].Start) {
				best = i
			}
		}
		return best
	}
	best := -1
	if mk.pid != 0 {
		// Exact, parallel-safe: attribute to the same worker's window.
		best = containingWindow(true)
	}
	if best == -1 {
		// No same-worker window (PID-less mock/windows, or the call came from
		// a child process): fall back to a pure timestamp scan — correct when
		// windows don't overlap, i.e. sequential record.
		best = containingWindow(false)
	}
	if best == -1 {
		return ""
	}
	return windows[best].Name
}

// mapOwners builds mappings.yaml from the owner the AGENT stamped on each
// capture, falling back to the timestamp correlation above where there is no
// stamp.
//
// The stamp is preferred because it is resolved at emit time, against scopes
// that are still open, by the process that owns them. correlateScopes runs
// after the fact over CLOSED windows only and has to infer the same thing from
// a timestamp — the two agree in the ordinary case, and where they do not the
// agent saw the live state and the correlation is guessing.
//
// It also has to be the stamp, not the correlation, once names are minted per
// owner: a mock named for owner A but mapped to test B would be a mapping the
// name itself contradicts.
//
// Disagreements are surfaced rather than swallowed — one WARN per mock, since
// each one is a mock a replay may serve to the wrong test.
func mapOwners(logger *zap.Logger, windows []models.ScopeWindow, mocks []capturedMock) map[string][]models.MockEntry {
	stamped := 0
	for _, mk := range mocks {
		if mk.owner != "" {
			stamped++
		}
	}
	if stamped == 0 {
		// Nothing carries a stamp: an agent that predates it, or a run whose
		// captures all fell outside every scope. Correlate the whole set.
		return correlateScopes(windows, mocks)
	}

	sortScopeWindows(windows)
	byTest := make(map[string][]models.MockEntry)
	disagreed := 0
	for _, mk := range mocks {
		correlated := scopeNameFor(windows, mk)
		owner := mk.owner
		if owner == "" {
			owner = correlated
		} else if correlated != "" && correlated != owner {
			disagreed++
			logger.Warn("the agent's capture-time owner and the CLI's timestamp correlation disagree about which test owns a mock; trusting the agent",
				zap.String("mock", mk.name),
				zap.String("agent_owner", owner),
				zap.String("correlated_owner", correlated))
		}
		if owner == "" {
			continue
		}
		byTest[owner] = append(byTest[owner], models.MockEntry{Name: mk.name})
	}

	// Record an entry for every scope that OPENED, including the ones that
	// captured nothing.
	//
	// Without this a test that makes no dependency calls is indistinguishable
	// at replay from a test that was never recorded: both are simply absent
	// from the table, and both report unmapped_scope. That ambiguity is what
	// forces a replay to guess -- it cannot fail a renamed or newly-added test
	// without also failing every legitimately call-free one.
	//
	// An empty entry says the difference out loud: "this test ran, and needed
	// nothing". Absence then means only one thing: no recording exists.
	for _, w := range windows {
		if w.Name == "" {
			continue
		}
		if _, seen := byTest[w.Name]; !seen {
			byTest[w.Name] = []models.MockEntry{}
		}
	}

	logger.Debug("built per-test mock mappings",
		zap.Int("mocks", len(mocks)),
		zap.Int("stamped_by_agent", stamped),
		zap.Int("disagreements", disagreed))
	return byTest
}

// drainTrailingMocks blocks until the agent has evidently finished handing over
// mocks: no new arrival for mockDrainQuiet, or the stream ended by itself, or
// mockDrainGrace elapsed, or the user interrupted. It must be called BEFORE the
// capture context is cancelled - see the call site.
func (m *mockService) drainTrailingMocks(ctx context.Context, consumerDone <-chan struct{}, mocksSeen *atomic.Int64) {
	deadline := time.After(mockDrainGrace)
	quiet := time.NewTimer(mockDrainQuiet)
	defer quiet.Stop()

	last := mocksSeen.Load()
	for {
		select {
		case <-consumerDone:
			// The agent closed the stream on its own; nothing left to wait for.
			return
		case <-ctx.Done():
			// Ctrl+C. Stop waiting and let the caller report what it has.
			return
		case <-deadline:
			m.logger.Debug("timed out draining trailing mocks after runner exit",
				zap.Int64("mocks_seen", mocksSeen.Load()))
			return
		case <-quiet.C:
			if now := mocksSeen.Load(); now != last {
				// Still arriving - reset and keep waiting.
				last = now
				quiet.Reset(mockDrainQuiet)
				continue
			}
			return
		}
	}
}

package mock

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	rec "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// mockDrainGrace bounds how long Record waits for the agent to hand over the
// last few mocks after the wrapped runner has exited. The outgoing stream
// closes when we cancel its context; this only guards against a mock whose
// agent-side parse completes in the window between the runner exiting and the
// stream tearing down.
const mockDrainGrace = 5 * time.Second

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
	if err := m.instrumentation.Setup(ctx, m.config.Command, models.SetupOptions{
		Container:   m.config.ContainerName,
		CommandType: m.config.CommandType,
		DockerDelay: m.config.BuildDelay,
		BuildDelay:  m.config.BuildDelay,
		Mode:        models.MODE_RECORD,
		MockMode:    true,
		ConfigPath:  m.config.ConfigPath,
	}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	// 2. Overwrite the named set in place: drop the previous mocks so the
	//    re-record is a clean rewrite, not an append.
	if err := m.mockDB.DeleteMocksForSet(persistCtx, name); err != nil {
		m.logger.Debug("no existing mock set to overwrite (or delete failed)", zap.String("mock-set", name), zap.Error(err))
	}
	// The per-test mapping is part of the set: UpsertBatch unions with what is
	// on disk and ResetCounterID below reissues the same mock-N names, so a
	// surviving mapping would attribute this run's mocks to the previous run's
	// tests.
	if m.mappingDB != nil {
		if err := m.mappingDB.Delete(persistCtx, name); err != nil {
			m.logger.Warn("failed to clear the previous per-test mock mappings; stale test entries may survive this re-record", zap.String("mock-set", name), zap.Error(err))
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
	consumerDone := make(chan struct{})
	go func() {
		defer utils.Recover(m.logger)
		defer close(consumerDone)
		for mk := range outgoing {
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
			recorded = append(recorded, capturedMock{name: mk.Name, ts: mk.Spec.ReqTimestampMock, pid: mk.SourcePID})
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

	// 6. Stop capturing and drain the last mocks.
	stopCapture()
	select {
	case <-consumerDone:
	case <-time.After(mockDrainGrace):
		m.logger.Debug("timed out draining trailing mocks after runner exit")
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
				byTest, attr := correlateScopes(windows, recorded)
				warnOnWeakAttribution(m.logger, windows, byTest, attr)
				if len(byTest) > 0 {
					if err := m.mappingDB.UpsertBatch(persistCtx, name, byTest); err != nil {
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

	m.logger.Info("recorded mocks", zap.Int("mocks", mockCount), zap.String("mock-set", name))
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
}

// correlateScopes buckets each recorded mock into the per-test scope window its
// request timestamp falls within, producing the mappings.yaml structure. A mock
// that matches no window (e.g. a boot-time handshake before the first scope)
// is left out of every test's mapping — it stays reusable/session-tier at
// replay, exactly as the timestamp-based fallback would treat it.
//
// When a mock carries a source PID it is attributed to the SAME worker's window
// (exact, so overlapping parallel windows don't steal each other's mocks). The
// proxy resolves that PID from the connection's origin up the /proc tree to the
// registered worker (Proxy.ResolveWorkerPID), so a call made by a CHILD of the
// worker — a browser's network process — still attributes exactly. It falls back
// to the timestamp scan when no worker registered, when the worker's process
// tree is not an ancestor of the caller (a runner that connects to an
// out-of-tree browser server), or when the runner's self-reported PID is from a
// different PID namespace than the agent's. The scan is exact for sequential
// record and best-effort under overlap; the caller warns when it was used while
// more than one worker was recording.
//
// The second return counts how each mock was resolved, for that warning.
func correlateScopes(windows []models.ScopeWindow, mocks []capturedMock) (map[string][]models.MockEntry, attribution) {
	// Sort windows by start so overlapping scopes resolve to the innermost
	// (latest-started) window deterministically.
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].Start.Before(windows[j].Start) })
	byTest := make(map[string][]models.MockEntry)
	// containingWindow returns the innermost (latest-started) window that contains
	// the mock's timestamp. When sameWorkerOnly is set (the mock carries a PID),
	// only windows recorded by that exact worker are considered — so overlapping
	// windows from OTHER parallel workers never steal the mock.
	containingWindow := func(mk capturedMock, sameWorkerOnly bool) int {
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
	var attr attribution
	for _, mk := range mocks {
		best := -1
		if mk.pid != 0 {
			// Exact, parallel-safe: attribute to the same worker's window.
			best = containingWindow(mk, true)
		}
		if best == -1 {
			// No same-worker window (PID-less mock/windows, or the call came from
			// a child process): fall back to a pure timestamp scan — correct when
			// windows don't overlap, i.e. sequential record.
			best = containingWindow(mk, false)
			if best != -1 {
				attr.byTimestamp++
			}
		} else {
			attr.byWorkerPID++
		}
		if best == -1 {
			continue
		}
		byTest[windows[best].Name] = append(byTest[windows[best].Name], models.MockEntry{Name: mk.name})
	}
	return byTest, attr
}

// attribution counts how correlateScopes resolved each mock it placed.
type attribution struct {
	byWorkerPID int
	byTimestamp int
}

// warnOnWeakAttribution surfaces the two failure modes that are otherwise
// completely silent, and only show up much later as a replay serving another
// test's data: mocks bucketed by timestamp while two workers were running
// concurrently, and tests that ended up with no mocks at all.
func warnOnWeakAttribution(logger *zap.Logger, windows []models.ScopeWindow, byTest map[string][]models.MockEntry, attr attribution) {
	if attr.byTimestamp > 0 && distinctWorkers(windows) > 1 {
		logger.Warn("parallel record: some mocks could not be attributed to the worker that made the call and were bucketed by timestamp instead; with several workers recording at once that bucketing is a guess, and a test can end up mapped to another test's mocks",
			zap.Int("mocks_attributed_by_worker", attr.byWorkerPID),
			zap.Int("mocks_guessed_by_timestamp", attr.byTimestamp),
			zap.String("next_step", "re-record with a single worker, or make the agent share the test runner's PID namespace (e.g. docker run --pid=host) so calls can be traced back to a worker"))
	}
	var empty []string
	seen := make(map[string]struct{}, len(windows))
	for _, w := range windows {
		if _, dup := seen[w.Name]; dup {
			continue
		}
		seen[w.Name] = struct{}{}
		if len(byTest[w.Name]) == 0 {
			empty = append(empty, w.Name)
		}
	}
	if len(empty) > 0 {
		logger.Warn("per-test mock mappings: some tests recorded no mocks, so at replay they will be served the whole set instead of their own recordings",
			zap.Strings("tests", empty),
			zap.String("next_step", "expected if those tests make no dependency calls; otherwise their calls were attributed to another test — see the parallel-record warning above"))
	}
}

// distinctWorkers counts the reporting workers across all windows. > 1 means the
// runner recorded in parallel, which is exactly when the timestamp fallback stops
// being sound: a sequential run's windows cannot overlap, so bucketing by time is
// exact, while concurrent workers' windows can, and then the bucket is a guess.
func distinctWorkers(windows []models.ScopeWindow) int {
	seen := make(map[uint32]struct{}, 4)
	for _, w := range windows {
		seen[w.PID] = struct{}{}
	}
	return len(seen)
}

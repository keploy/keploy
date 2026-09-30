package mock

import (
	"context"
	"errors"
	"fmt"
	"go.keploy.io/server/v3/config"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

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
	requests := m.requests()
	m.logger.Info("Recording mocks for your test command",
		zap.String("mock-set", name),
		zap.String("command", m.userCommand))

	// The caller's context: only ITS cancellation is the user's Ctrl+C. The
	// errgroup below cancels the context it derives whenever any goroutine in
	// it fails -- the agent process exiting is one of them -- so testing that
	// one reported keploy's own failure as an interrupt: exit 0, the test
	// command never run, and a CI job gating on the exit code went green.
	parent := ctx
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
		Container:        m.config.ContainerName,
		FromContainer:    m.config.FromContainer,
		CommandType:      m.config.CommandType,
		DockerDelay:      m.config.BuildDelay,
		BuildDelay:       m.config.BuildDelay,
		Mode:             models.MODE_RECORD,
		MockMode:         true,
		ConfigPath:       m.config.ConfigPath,
		PassThroughPorts: config.GetByPassPorts(m.config),
		RecordRequests:   requests,
	}); err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// 2. Docker compose inverts this function's normal order: arming the
	//    capture (step 4) needs a live agent, but under compose the agent is a
	//    service inside the compose project keploy generates, so it does not
	//    exist until the wrapped `docker compose up` runs. startComposeApp
	//    brings the project up and waits for the agent to answer; the app
	//    itself stays parked at the agent's healthcheck until step 5 releases
	//    it, so nothing it does escapes the capture. Its exit is collected at
	//    step 7 instead of being started there.
	//
	//    Without this the run dialled an agent that was never started, failed
	//    to arm the capture, and ended having recorded nothing — while the app
	//    itself never came up at all.
	composeAppExit, err := m.startComposeApp(ctx, errGrp, "record")
	if err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed to bring up the keploy-agent compose service"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// 3. Overwrite the named set in place: drop the previous mocks so the
	//    re-record is a clean rewrite, not an append.
	restore, existed := m.saveSet(name)
	keep := false
	defer func() { restore(keep) }()
	if err := m.mockDB.DeleteMocksForSet(persistCtx, name); err != nil {
		m.logger.Debug("no existing mock set to overwrite (or delete failed)", zap.String("mock-set", name), zap.Error(err))
	}
	m.mockDB.ResetCounterID()
	m.deleteMappings(persistCtx, name)
	if requests {
		// The old cases stay until the run has ended so the new ones are numbered after them: a flow
		// re-recorded on its own must not reuse the names of the flows the set carries.
		defer m.deleteCases(persistCtx, name, m.caseNames(persistCtx, name))
	}

	// 4. Arm the record proxy and stream captured mocks.
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
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed to start capturing outgoing calls"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
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
			kind := mk.Spec.Metadata["type"]
			recorded = append(recorded, capturedMock{name: mk.Name, ts: mk.Spec.ReqTimestampMock, pid: mk.SourcePID, boot: kind == "config" || kind == "connection"})
		}
	}()

	// 4b. With --record-requests, also store the app's incoming requests as test cases.
	capture := m.captureCases(captureCtx, persistCtx, name, &mocksSeen, requests)

	// 5. Release the compose app now that the capture is armed and draining.
	//    This is the post-arm half of step 2 — see releaseComposeApp.
	if err := m.releaseComposeApp(ctx); err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed to release the app behind the keploy-agent healthcheck"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// 6. Optional record timer.
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

	// 7. Block until the wrapped runner exits. Its exit — clean or failing —
	//    is the NORMAL end of a mock recording, not an app crash. Under
	//    compose it was already started at step 2, so wait on that same exit
	//    rather than starting it a second time.
	//    A plain receive is the same wait the native branch does: Run returns
	//    only once the app is fully down (its errgroup Wait is deferred), and
	//    the sender is the goroutine running exactly that Run.
	var appErr models.AppError
	if composeAppExit != nil {
		appErr = <-composeAppExit
	} else {
		appErr = m.instrumentation.Run(ctx, models.RunOptions{AppCommand: m.config.Command})
	}

	// 8. Drain the trailing mocks, THEN stop capturing.
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
	capture.wait(mockDrainGrace)

	if parent.Err() != nil { // user Ctrl+C
		keep = true
		m.logger.Info("recording stopped", zap.Int("mocks", mockCount), zap.String("mock-set", name))
		return nil
	}
	// Not the user: the agent died under the test command. What was captured
	// is on disk, but it is not a whole recording, and saying "recorded" over
	// it (or exiting 0) would vouch for one. Natively that fails the run's own
	// errgroup; under compose the agent is a service in the project, its death
	// stops the project -- test command included, whose exit is then compose's
	// stop and not its verdict -- and only the agent's container says so.
	cause := m.composeAgentFailure()
	if cause == nil && ctx.Err() != nil {
		cause = context.Cause(ctx)
	}
	if cause != nil {
		stopReason = "the recording did not finish"
		utils.LogError(m.logger, cause, stopReason, zap.Int("mocks", mockCount), zap.String("mock-set", name))
		return fmt.Errorf("%s: %w", stopReason, cause)
	}

	// 9. Correlate per-test scope windows into mappings.yaml (best-effort).
	var windows []models.ScopeWindow
	if m.mappingDB != nil {
		windows = m.agentWindows(persistCtx)
	}
	if err := repeatedScope(windows, existed); err != nil {
		m.propagateExit(appErr, "record")
		m.logger.Error(err.Error())
		return err
	}
	keep = true
	if m.mappingDB != nil {
		if len(windows) > 0 {
			byTest := correlateScopes(windows, recorded)
			byCase := correlateCases(windows, recorded, capture.list(), stepWindows(windows))
			for _, w := range windows {
				if _, ok := byTest[w.Name]; !ok {
					byTest[w.Name] = nil
				}
			}
			if len(byTest) > 0 {
				if err := m.mappingDB.UpsertBatch(persistCtx, name, byTest); err != nil {
					m.logger.Warn("failed to write per-test mappings; replay will serve the whole set per test", zap.Error(err))
				} else {
					m.logger.Info("wrote per-test mock mappings", zap.Int("tests", len(byTest)), zap.String("mock-set", name))
				}
			}
			m.upsertCases(persistCtx, name, byCase, startupMocks(windows, recorded))
		}
	}

	if err := m.hooks.AfterRecordingComplete(persistCtx, &rec.RecordingCompleteContext{TestSetID: name, Path: m.config.Path}); err != nil {
		m.logger.Warn("AfterRecordingComplete hook failed", zap.Error(err), zap.String("mock-set", name))
	}

	// 10. Publish the set to the store (registry upload in enterprise; no-op on files).
	if !runnerPassed(appErr) {
		m.logger.Warn("tests failed; the recording was kept locally and not published", zap.String("mock-set", name))
	} else if err := m.store.Push(persistCtx, name); err != nil {
		m.logger.Warn("failed to publish mock set to the store", zap.String("mock-set", name), zap.Error(err))
	}

	m.logger.Info("recorded mocks", zap.Int("mocks", mockCount), zap.String("mock-set", name))
	if capture != nil {
		m.logger.Info("recorded test cases", zap.Int("cases", len(capture.list())), zap.String("mock-set", name))
	}
	if mockCount == 0 {
		m.logger.Warn("no outgoing calls were captured; the runner made no mockable dependency calls, or its traffic was not intercepted",
			zap.String("next_step", "confirm the test command actually calls an external dependency (HTTP, MySQL, ...), and on macOS run it via a docker command"))
	}

	// 11. Propagate the runner's exit code so a CI 're-record on merge' job
	//     fails when the tests fail.
	m.propagateExit(appErr, "record")
	return nil
}

func runnerPassed(appErr models.AppError) bool {
	switch appErr.AppErrorType {
	case models.ErrAppStopped, models.ErrCtxCanceled, "":
		return true
	}
	return false
}

// agentWindows reads the windows the runner itself posted to the agent's scope API; none is not an error.
func (m *mockService) agentWindows(ctx context.Context) []models.ScopeWindow {
	reader, ok := m.instrumentation.(ScopeReader)
	if !ok {
		return nil
	}
	scopeCtx, cancel := context.WithTimeout(ctx, agentEpilogueTimeout)
	defer cancel()
	windows, err := reader.GetScopeWindows(scopeCtx)
	if err != nil {
		m.logger.Debug("failed to read per-test scope windows from the agent", zap.Error(err))
		return nil
	}
	return windows
}

func (m *mockService) saveSet(name string) (func(keep bool), bool) {
	if m.config.Path == "" {
		return func(bool) {}, true
	}
	set := filepath.Join(m.config.Path, name)
	saved := filepath.Join(m.config.Path, "."+name+".previous")
	_ = os.RemoveAll(saved)
	err := os.CopyFS(saved, os.DirFS(set))
	absent := errors.Is(err, fs.ErrNotExist)
	if err != nil {
		_ = os.RemoveAll(saved)
		if !absent {
			m.logger.Warn("could not keep a copy of the set before re-recording it; a refused recording cannot be undone", zap.String("mock-set", name), zap.Error(err))
		}
	}
	return func(keep bool) {
		switch {
		case keep:
			_ = os.RemoveAll(saved)
		case absent:
			_ = os.RemoveAll(set)
		case err == nil:
			if rmErr := os.RemoveAll(set); rmErr != nil {
				m.logger.Warn("could not put the previous recording back", zap.String("mock-set", name), zap.Error(rmErr))
				return
			}
			if mvErr := os.Rename(saved, set); mvErr != nil {
				m.logger.Warn("could not put the previous recording back; it is kept at "+saved, zap.String("mock-set", name), zap.Error(mvErr))
			}
		}
	}, !absent
}

// caseNames lists the test cases the set holds before a re-record.
func (m *mockService) caseNames(ctx context.Context, name string) []string {
	if m.testDB == nil {
		return nil
	}
	tcs, err := m.testDB.GetTestCases(ctx, name)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(tcs))
	for _, tc := range tcs {
		ids = append(ids, tc.Name)
	}
	return ids
}

// deleteCases drops the set's old test cases so a re-record ends clean, as it does for mocks.
func (m *mockService) deleteCases(ctx context.Context, name string, ids []string) {
	if m.testDB == nil || len(ids) == 0 {
		return
	}
	if err := m.testDB.DeleteTests(ctx, name, ids); err != nil {
		m.logger.Debug("failed to delete the old test cases", zap.String("mock-set", name), zap.Error(err))
	}
}

// upsertCases writes which test cases each flow produced into the mapping.
func (m *mockService) upsertCases(ctx context.Context, name string, byCase map[string]models.MappedTestCase, startup []models.MockEntry) {
	if len(byCase) == 0 && len(startup) == 0 {
		return
	}
	mapper, ok := m.mappingDB.(CaseMapper)
	if !ok {
		m.logger.Warn("the mapping store cannot record test cases per flow", zap.String("mock-set", name))
		return
	}
	if err := mapper.UpsertCases(ctx, name, byCase, startup); err != nil {
		m.logger.Warn("failed to write per-flow test cases", zap.Error(err))
		return
	}
	m.logger.Info("wrote per-flow test cases", zap.Int("flows", len(byCase)), zap.String("mock-set", name))
}

// deleteMappings drops the set's old per-test mappings so a re-record cannot leave tests pointing at renamed mocks.
func (m *mockService) deleteMappings(ctx context.Context, name string) {
	if m.mappingDB == nil {
		return
	}
	deleter, ok := m.mappingDB.(MappingDeleter)
	if !ok {
		m.logger.Warn("the mapping store cannot delete, so old per-test mappings may linger", zap.String("mock-set", name))
		return
	}
	if err := deleter.Delete(ctx, name); err != nil {
		m.logger.Debug("no existing mappings to overwrite (or delete failed)", zap.String("mock-set", name), zap.Error(err))
	}
}

// capturedMock is one recorded mock's name + request timestamp + source worker
// PID, used to correlate mocks into per-test scope windows.
type capturedMock struct {
	name string
	ts   time.Time
	pid  uint32 // source worker PID (0 if unknown); enables exact parallel attribution
	end  time.Time
	boot bool
}

// correlateScopes buckets each recorded mock into the per-test scope window its
// request timestamp falls within, producing the mappings.yaml structure. A mock
// that matches no window (e.g. a boot-time handshake before the first scope)
// is left out of every test's mapping — it stays reusable/session-tier at
// replay, exactly as the timestamp-based fallback would treat it.
//
// When a mock carries a source PID it is attributed to the SAME worker's window
// (exact, so overlapping parallel windows don't steal each other's mocks). That
// PID match is exact only when the worker made the call itself and the agent
// shares its PID namespace (the normal `keploy mock <cmd>` wrap); a call made by
// a CHILD of the worker, or a containerized/cross-namespace worker whose
// self-reported PID differs from the kernel PID, falls back to the timestamp
// scan — which is exact for sequential record and best-effort under overlap.
func correlateScopes(windows []models.ScopeWindow, mocks []capturedMock) map[string][]models.MockEntry {
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
		}
		if best == -1 {
			continue
		}
		byTest[windows[best].Name] = append(byTest[windows[best].Name], models.MockEntry{Name: mk.name})
	}
	return byTest
}

// correlateCases lists each flow's test cases by the window their request fell in; a case outside every window stays unmapped.
func correlateCases(windows []models.ScopeWindow, mocks, cases []capturedMock, steps []models.ScopeWindow) map[string]models.MappedTestCase {
	byTest := make(map[string]models.MappedTestCase)
	spans := make(map[string]models.ScopeWindow, len(cases))
	for _, c := range cases {
		spans[c.name] = models.ScopeWindow{Name: c.name, Start: c.ts, End: c.end}
	}
	for test, entries := range correlateScopes(windows, cases) {
		tc := models.MappedTestCase{CaseSteps: make(map[string]string, len(entries))}
		for _, e := range entries {
			tc.Cases = append(tc.Cases, e.Name)
			tc.CaseSteps[e.Name] = containing(steps, spans[e.Name].Start)
		}
		byTest[test] = tc
	}
	var own []capturedMock
	at := make(map[string]time.Time, len(mocks))
	for _, mk := range mocks {
		if !mk.boot {
			own = append(own, mk)
			at[mk.name] = mk.ts
		}
	}
	holder := make(map[string]string, len(cases))
	inFlow := make(map[string][]models.ScopeWindow)
	for test, tc := range byTest {
		for _, c := range tc.Cases {
			holder[c] = test
			inFlow[flowOf(windows, test)] = append(inFlow[flowOf(windows, test)], spans[c])
		}
	}
	for test, entries := range correlateScopes(windows, own) {
		for _, e := range entries {
			c := containing(inFlow[flowOf(windows, test)], at[e.Name])
			if c == "" {
				continue
			}
			tc := byTest[holder[c]]
			if tc.CaseMocks == nil {
				tc.CaseMocks = map[string][]string{}
			}
			tc.CaseMocks[c] = append(tc.CaseMocks[c], e.Name)
			byTest[holder[c]] = tc
		}
	}
	for _, tc := range byTest {
		for _, names := range tc.CaseMocks {
			sort.SliceStable(names, func(i, j int) bool { return at[names[i]].Before(at[names[j]]) })
		}
	}
	return byTest
}

func flowOf(windows []models.ScopeWindow, test string) string {
	flow := test
	for _, w := range windows {
		if strings.HasPrefix(test, w.Name+"/") && len(w.Name) < len(flow) {
			flow = w.Name
		}
	}
	return flow
}

func startupMocks(windows []models.ScopeWindow, mocks []capturedMock) []models.MockEntry {
	var out []models.MockEntry
	for _, mk := range mocks {
		if mk.boot || containing(windows, mk.ts) == "" {
			out = append(out, models.MockEntry{Name: mk.name})
		}
	}
	return out
}

func containing(windows []models.ScopeWindow, at time.Time) string {
	best := -1
	for i, w := range windows {
		if at.Before(w.Start) || at.After(w.End) {
			continue
		}
		if best == -1 || w.Start.After(windows[best].Start) {
			best = i
		}
	}
	if best == -1 {
		return ""
	}
	return windows[best].Name
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

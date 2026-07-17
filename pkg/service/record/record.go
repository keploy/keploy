// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/telemetry"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	mappingDb       MappingDb
	telemetry       Telemetry
	instrumentation Instrumentation
	testSetConf     TestSetConfig
	config          *config.Config
	hooks           RecordHooks

	// cleanups is run in LIFO order from Start's defer. Downstream
	// builds and internal subsystems register shutdown work here
	// rather than each one type-asserting its own io.Closer into the
	// store interfaces. Register with RegisterCleanup.
	cleanupMu sync.Mutex
	cleanups  []func() error
}

// RegisterCleanup appends a shutdown callback that Recorder.Start's
// defer will drain in LIFO order. Thread-safe. Callbacks should be
// idempotent — Recorder may be restarted in some flows.
func (r *Recorder) RegisterCleanup(fn func() error) {
	if fn == nil {
		return
	}
	r.cleanupMu.Lock()
	r.cleanups = append(r.cleanups, fn)
	r.cleanupMu.Unlock()
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, mappingDB MappingDb, telemetry Telemetry, instrumentation Instrumentation, testSetConf TestSetConfig, hooks RecordHooks, config *config.Config) Service {
	if hooks == nil {
		hooks = BaseRecordHooks{}
	}
	return &Recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDb:       mappingDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		testSetConf:     testSetConf,
		config:          config,
		hooks:           hooks,
	}
}

// SetRecordHooks replaces the current hooks. Mirrors SetTestHooks on the Replayer.
func (r *Recorder) SetRecordHooks(hooks RecordHooks) {
	if hooks != nil {
		r.hooks = hooks
	}
}

// resolveMappingEntries correlates a test's mapping tempIDs to the persisted
// MockEntries in correlationMap, dropping each consumed tempID as it goes. It
// EXCLUDES async-egress mocks (those in asyncMockIDs): they are served at replay
// by the async engine from the complete corpus, so they must never be recorded
// as a testcase's per-test mock — even when a background egress's timestamp
// happened to fall inside the testcase's request window (the agent's mapper bins
// purely by timestamp and has no async awareness). The Mock loop stores the
// async marker before it publishes the tempID to correlationMap, so a resolved
// tempID always has its async status already decided.
func (r *Recorder) resolveMappingEntries(mapping models.TestMockMapping, correlationMap, asyncMockIDs *sync.Map) []models.MockEntry {
	var realMockEntries []models.MockEntry
	for _, tempID := range mapping.MockIDs {
		var realEntry models.MockEntry
		found := false
		// Simple retry loop (fast spin) to wait for the Mock Loop.
		for i := 0; i < 50; i++ { // Wait up to ~500ms
			if val, ok := correlationMap.Load(tempID); ok {
				realEntry = val.(models.MockEntry)
				found = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !found {
			r.logger.Error("Failed to correlate mock mapping",
				zap.String("test", mapping.TestName),
				zap.String("tempMockID", tempID),
				zap.String("next_step", "ensure mapping store is enabled, avoid high parallelism, or re-record if mappings are inconsistent"))
			continue
		}
		correlationMap.Delete(tempID)
		if _, isAsync := asyncMockIDs.Load(tempID); isAsync {
			continue
		}
		realMockEntries = append(realMockEntries, realEntry)
	}
	return realMockEntries
}

// GetRecordHooks returns the current hooks.
func (r *Recorder) GetRecordHooks() RecordHooks {
	return r.hooks
}

func (r *Recorder) Start(ctx context.Context) error {

	r.logger.Debug("Starting Keploy recording... Please wait.")

	sessionStart := time.Now()

	// Auto-register mockDB.Close if it implements io.Closer. MockYaml
	// implements Close unconditionally: in gob mode it drains the
	// async writer and flushes the file; in yaml mode it is a no-op
	// (gobStop is nil, so Close returns nil immediately). Registered
	// via RegisterCleanup so it drains in LIFO order alongside any
	// other subsystems the caller has registered (telemetry flush,
	// mapping-DB sync, etc.).
	if closer, ok := r.mockDB.(io.Closer); ok {
		r.RegisterCleanup(closer.Close)
	}

	// Drain all registered cleanups in LIFO order on return. Errors
	// are logged but do not abort subsequent cleanups — each subsystem
	// gets its flush regardless of what came before.
	defer func() {
		r.cleanupMu.Lock()
		cleanups := r.cleanups
		r.cleanups = nil
		r.cleanupMu.Unlock()
		for i := len(cleanups) - 1; i >= 0; i-- {
			if err := cleanups[i](); err != nil {
				// A cleanup error usually means a mock-file flush or
				// telemetry-drain returned non-nil — the session data
				// is still on disk (the writer buffers drain before
				// Close returns err on inner failures), but the tail
				// batch may be incomplete. Log at Error so the
				// operator sees it on exit summary; include cleanup
				// index so `record cleanup N failed` is actionable
				// against the known RegisterCleanup call sites.
				r.logger.Error("record cleanup returned an error; inspect the mock file for a truncated tail and check disk space/permissions at the recording output directory",
					zap.Int("cleanupIndex", i),
					zap.Error(err))
			}
		}
	}()

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	errGrp, _ := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	runAppErrGrp, _ := errgroup.WithContext(ctx)
	runAppCtx := context.WithoutCancel(ctx)
	runAppCtx, runAppCtxCancel := context.WithCancel(runAppCtx)

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	// Propagate parent context cancellation to setupCtx
	// This ensures that when Ctrl+C is pressed, setupCtx is cancelled immediately
	go func() {
		<-ctx.Done()
		setupCtxCancel()
	}()

	reqErrGrp, _ := errgroup.WithContext(ctx)
	reqCtx := context.WithoutCancel(ctx)
	reqCtx, reqCtxCancel := context.WithCancel(reqCtx)
	reqCtx = context.WithValue(reqCtx, models.ErrGroupKey, reqErrGrp)

	var stopReason string
	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var insertTestErrChan = make(chan error, 10)
	var insertMockErrChan = make(chan error, 10)
	var newTestSetID string
	var err error
	var testCount = 0
	var mockCountMap = make(map[string]int)
	// mockCountMap is written by the mock-consumer goroutine and read at
	// teardown for telemetry. Guard both with a mutex so that if teardown
	// ever runs while the consumer is still draining (e.g. a force-shutdown),
	// it can't trigger a concurrent map read/write. correlationMap beside it
	// is already a sync.Map.
	var mockCountMapMu sync.Mutex
	domainSet := telemetry.NewDomainSet()
	var recordingStarted bool

	// Deferred-orphan revoke bookkeeping. revokedNames collects TC names the
	// agent signalled via Kind=RevokedTests control frames (their owned mock
	// was capacity-dropped AFTER the TC streamed); insertedNames records which
	// TCs actually persisted. At finalize we delete the intersection so a
	// revoke that raced ahead of its TC's persistence is still caught. Both are
	// written only by the mock/TC consumer goroutines and read at teardown
	// AFTER those goroutines drain, but the mutexes are cheap insurance against
	// a force-shutdown teardown racing an in-flight consumer.
	revokedNames := map[string]struct{}{}
	var revokedMu sync.Mutex
	insertedNames := map[string]struct{}{}
	var insertedMu sync.Mutex

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}

		r.logger.Info("Stopping Keploy recording...")

		// Notify the agent that we are shutting down gracefully
		// This will cause connection errors to be logged as debug instead of error.
		// Bound it: an up-but-unresponsive agent (still booting under contention)
		// must not block teardown — the path SIGINT takes — indefinitely.
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}
		notifyCancel()
		// Pcap + keylog flow over long-lived HTTP streams started in
		// Start() right after the agent's broadcaster came up. The
		// streams unwind on their own when the agent closes the
		// response or the recorder context is cancelled — there is
		// no on-disk file on the agent and no fetch step here.

		// Bounded drains: a goroutine wedged under contention that ignores its
		// cancel() must not hang teardown forever (which would swallow SIGINT and
		// keep the process alive until an external SIGKILL). See utils.DrainErrGroup.
		runAppCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "record-app", runAppErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop application")
		}

		reqCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "record-req", reqErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop request processing")
		}

		setupCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "record-setup", setupErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop setup execution, that covers init container")
		}

		if err := utils.DrainErrGroup(r.logger, "record", errGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}

		// Deferred-orphan revoke: delete TCs whose owned mock was capacity-dropped
		// AFTER the TC streamed (the agent signalled them live via RevokedTests
		// control frames). Applied here — after all inserts drained — so a revoke
		// that raced ahead of its TC's persistence is still caught by the intersect.
		// Snapshot both sets under their OWN locks before intersecting. On the
		// DrainErrGroup TIMEOUT path a wedged consumer goroutine can still be
		// writing these maps (DrainErrGroup returns after its timeout WITHOUT
		// joining a goroutine that ignores cancellation — see utils/drain.go),
		// so an unguarded read here could hit a fatal "concurrent map read and
		// map write". The two maps are never locked together elsewhere, so
		// snapshot each independently (lock, copy, unlock) — no nesting, no
		// ordering hazard.
		revokedMu.Lock()
		revokedSnap := make([]string, 0, len(revokedNames))
		for n := range revokedNames {
			revokedSnap = append(revokedSnap, n)
		}
		revokedMu.Unlock()

		var toDelete []string
		insertedMu.Lock()
		for _, n := range revokedSnap {
			if _, ok := insertedNames[n]; ok {
				toDelete = append(toDelete, n)
			}
		}
		insertedMu.Unlock()
		if len(toDelete) > 0 {
			// DeleteTests is on the concrete *testdb.TestYaml but NOT on the record
			// TestDB interface (enterprise implements that interface; don't break it).
			// Reach it by runtime assertion; degrade to a warning if unavailable.
			if td, ok := r.testDB.(interface {
				DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
			}); ok {
				deleted := 0
				for _, n := range toDelete {
					if err := td.DeleteTests(ctx, newTestSetID, []string{n}); err != nil {
						r.logger.Warn("deferred-orphan revoke: failed to delete a capacity-orphaned test case; leaving it in the set",
							zap.String("testSetID", newTestSetID), zap.String("testCase", n), zap.Error(err))
						continue
					}
					deleted++
				}
				if deleted > 0 {
					// Guard under insertedMu so this decrement can't race the
					// (possibly still-live, on the drain-timeout path) testCount++
					// in the TC-insert loop, which now runs under the same lock.
					insertedMu.Lock()
					testCount -= deleted
					insertedMu.Unlock()
					r.logger.Info("deferred-orphan revoke: removed capacity-orphaned test cases whose mock was dropped after streaming",
						zap.Int("revoked", deleted), zap.String("testSetID", newTestSetID))
				}
			} else {
				r.logger.Warn("deferred-orphan revoke: testDB does not support DeleteTests; leaving orphaned test cases in the set",
					zap.Int("count", len(toDelete)))
			}
		}

		totalMocks := 0
		if recordingStarted {
			mockCountMapMu.Lock()
			mockCountSnapshot := make(map[string]int, len(mockCountMap))
			for k, v := range mockCountMap {
				mockCountSnapshot[k] = v
			}
			mockCountMapMu.Unlock()
			r.telemetry.RecordedTestSuite(newTestSetID, testCount, mockCountSnapshot, map[string]interface{}{
				"host-domains": domainSet.ToSlice(),
			})
			for _, c := range mockCountSnapshot {
				totalMocks += c
			}
		}
		// Emit the session summary on every graceful stop: either recording
		// actually started, OR an error path set a stopReason before frames
		// were established (setup/agent-not-ready/testset-lookup failures that
		// previously emitted nothing — the highest-signal "stuck" cases).
		// "completed" for a clean exit / user Ctrl+C, "aborted" when a
		// stopReason was set; stop_reason carries the categorized cause.
		if recordingStarted || stopReason != "" {
			status := "completed"
			if stopReason != "" {
				status = "aborted"
			}
			r.telemetry.RecordSessionCompleted(int64(testCount), int64(totalMocks), time.Since(sessionStart).Milliseconds(), status, stopReason)
		}
		if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
			s.Shutdown()
		}
	}()

	// appErrChan is intentionally NOT closed by Start. The app-runner goroutine
	// spawned in runAppErrGrp (the DockerCompose branch at ~308 and the
	// non-compose branch at ~518) sends the app's exit error on it and can
	// still be running when Start returns during shutdown: on SIGTERM the app
	// exits with "signal: terminated" (not ErrCtxCanceled), so the goroutine
	// reaches its `appErrChan <- runAppError` send. Closing the channel here
	// raced that send and panicked with "send on closed channel" (seen in the
	// pulsar-basetopic teardown). The sole consumer is a single receive in the
	// select below that does not depend on a close, and the size-1 buffer
	// absorbs the lone send (the two sender branches are mutually exclusive),
	// so leaving the channel open to be GC'd is correct and race-free.
	defer close(insertTestErrChan)
	defer close(insertMockErrChan)

	newTestSetID, err = r.GetNextTestSetID(ctx)
	if err != nil {
		stopReason = "failed to get new test-set id"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	// Create config.yaml if metadata is provided
	if r.config.Record.Metadata != "" && r.testSetConf != nil {
		r.createConfigWithMetadata(ctx, newTestSetID)
	}

	//checking for context cancellation as we don't want to start the instrumentation if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	passPortsUint := config.GetByPassPorts(r.config)
	passPortsUint32 := make([]uint32, len(passPortsUint)) // slice type of uint32
	for i, port := range passPortsUint {
		passPortsUint32[i] = uint32(port)
	}

	memoryLimit := uint64(0)
	if r.config.CommandType == string(utils.DockerRun) || r.config.CommandType == string(utils.DockerCompose) {
		memoryLimit = r.config.Record.MemoryLimit
	}

	// Instrument will setup the environment and start the hooks and proxy
	err = r.instrumentation.Setup(setupCtx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerDelay: r.config.BuildDelay, Mode: models.MODE_RECORD, CommandType: r.config.CommandType, EnableTesting: false, GlobalPassthrough: r.config.Record.GlobalPassthrough, CapturePackets: r.config.Record.CapturePackets, OpportunisticTLSIntercept: r.config.Record.OpportunisticTLSIntercept, ChannelBindingShim: r.config.Record.ChannelBindingShim, BuildDelay: r.config.BuildDelay, PassThroughPorts: passPortsUint, MemoryLimit: memoryLimit, ConfigPath: r.config.ConfigPath, EnableSampling: r.config.Record.EnableSampling, RecordBufferMaxMemoryPerConn: r.config.Record.RecordBuffer.MaxMemoryPerConnection, RecordBufferQueueSize: r.config.Record.RecordBuffer.QueueSize})

	if err != nil {
		// If context was cancelled (user pressed Ctrl+C), return gracefully without error
		if ctx.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	r.logger.Debug("Command type:", zap.String("commandType", r.config.CommandType))

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Info("Waiting for keploy-agent to be ready for docker compose...", zap.String("Agent-uri", r.config.Agent.AgentURI))

		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			if (runAppError.AppErrorType == models.ErrCtxCanceled || runAppError == models.AppError{}) {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})

		// Aligned with the agent's own healthcheck budget; a fixed 120s wait gave
		// up while the agent container was still starting under CI daemon
		// contention. See pkg.AgentReadyTimeout (KEPLOY_AGENT_READY_TIMEOUT).
		agentCtx, cancel := context.WithTimeout(ctx, pkg.AgentReadyTimeout())
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, r.logger, r.config.Agent.AgentURI, agentReadyCh, 1*time.Second)

		select {
		case <-ctx.Done():
			// Parent context cancelled (user pressed Ctrl+C)
			return ctx.Err()
		case <-agentCtx.Done():
			return fmt.Errorf("keploy-agent did not become ready in time")
		case <-agentReadyCh:
		}
	}

	r.logger.Debug("Agent is ready. Starting to fetch test cases and mocks...")

	var correlationMap sync.Map
	// asyncMockIDs holds tempIDs of async mocks (Spec.Async set); resolveMappingEntries uses it
	// to keep async-egress mocks out of the per-test mappings (see its doc).
	var asyncMockIDs sync.Map
	// fetching test cases and mocks from the application and inserting them into the database
	frames, err := r.GetTestAndMockChans(reqCtx)
	if err != nil {
		stopReason = "failed to get data frames"
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf("%s", stopReason)
	}
	recordingStarted = true
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Kick off the pcap + keylog streams. GetTestAndMockChans above
	// has already triggered Proxy.Record() on the agent, which
	// installs the broadcaster — so the stream subscribes against a
	// live capture. The goroutine ends when ctx is cancelled
	// (recording stop) or when the agent's HTTP server closes the
	// response. Any error is logged but never fails the recording.
	if r.config.Record.CapturePackets {
		destDir := filepath.Join(r.config.Path, newTestSetID)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			r.logger.Warn("failed to create test-set dir for pcap stream; continuing without pcap",
				zap.String("destDir", destDir), zap.Error(err))
		} else {
			errGrp, _ := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
			if errGrp != nil {
				errGrp.Go(func() error {
					if err := r.instrumentation.StreamPcapArtifacts(ctx, destDir); err != nil {
						utils.LogError(r.logger, err, "pcap stream ended with error",
							zap.String("destDir", destDir))
					}
					return nil
				})
			}
		}
	}

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Debug("Making keploy-agent ready for docker compose...")

		err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}
	}

	r.logger.Info("Keploy agent is ready to record test cases and mocks.")

	r.mockDB.ResetCounterID() // Reset mock ID counter for each recording session
	errGrp.Go(func() error {
		for testCase := range frames.Incoming {
			// Skip curl generation for either form data requests or large body (>1MB)
			if len(testCase.HTTPReq.Body) <= 1*1024*1024 && len(testCase.HTTPReq.Form) == 0 {
				testCase.Curl = pkg.MakeCurlCommand(testCase.HTTPReq)
			}
			domainSet.AddAll(telemetry.ExtractDomainsFromTestCase(testCase))
			if hookErr := r.hooks.BeforeTestCaseInsert(ctx, &TestCaseContext{
				TestCase: testCase, TestSetID: newTestSetID,
			}); hookErr != nil {
				r.logger.Error("BeforeTestCaseInsert hook failed; recording will continue but hook side-effects may be missing. Check your RecordHooks implementation.",
					zap.Error(hookErr),
					zap.String("testSetID", newTestSetID),
					zap.String("testCaseName", testCase.Name))
			}
			err := r.testDB.InsertTestCase(ctx, testCase, newTestSetID, true)
			if err != nil {
				if ctx.Err() == context.Canceled {
					continue
				}
				insertTestErrChan <- err
			} else {
				// testCount and insertedNames are both TC-insert state; write
				// them under one insertedMu section so the finalize revoke block
				// (which reads insertedNames and adjusts testCount, possibly
				// while this goroutine is still live on the drain-timeout path)
				// can't race them.
				insertedMu.Lock()
				testCount++
				insertedNames[testCase.Name] = struct{}{}
				insertedMu.Unlock()
				r.telemetry.RecordedTestAndMocks()
				if hookErr := r.hooks.AfterTestCaseInsert(ctx, &TestCaseContext{
					TestCase: testCase, TestSetID: newTestSetID,
				}); hookErr != nil {
					r.logger.Error("AfterTestCaseInsert hook failed; test case was recorded successfully but post-insert hook side-effects may be missing. Check your RecordHooks implementation.",
						zap.Error(hookErr),
						zap.String("testSetID", newTestSetID),
						zap.String("testCaseName", testCase.Name))
				}
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		for mock := range frames.Outgoing {
			// Deferred-orphan revoke: a reserved-Kind control frame, NOT a mock.
			// Divert it into the revoke set (applied at finalize) BEFORE any
			// domain-extraction / hook / InsertMock / correlation work — it
			// carries no traffic and must never be persisted.
			if mock.GetKind() == string(models.RevokedTests) {
				if mock.Spec.Metadata != nil {
					for _, n := range strings.Split(mock.Spec.Metadata["revoked_tests"], ",") {
						if n = strings.TrimSpace(n); n != "" {
							revokedMu.Lock()
							revokedNames[n] = struct{}{}
							revokedMu.Unlock()
						}
					}
				}
				continue
			}
			domainSet.AddAll(telemetry.ExtractDomainsFromMock(mock))
			tempID := mock.Name
			if hookErr := r.hooks.BeforeMockInsert(ctx, &MockContext{
				Mock: mock, TestSetID: newTestSetID,
			}); hookErr != nil {
				r.logger.Error("BeforeMockInsert hook failed; recording will continue but hook side-effects may be missing. Check your RecordHooks implementation.",
					zap.Error(hookErr),
					zap.String("testSetID", newTestSetID),
					zap.String("mockName", mock.Name),
					zap.String("mockKind", mock.GetKind()))
			}
			// The AsyncRecorder hook sets Spec.Async in BeforeMockInsert above;
			// remember it so the mapping goroutine below never per-test maps it.
			if mock.IsAsync() {
				asyncMockIDs.Store(tempID, struct{}{})
			}
			err := r.mockDB.InsertMock(ctx, mock, newTestSetID)
			if err != nil {
				if ctx.Err() == context.Canceled {
					continue
				}
				insertMockErrChan <- err
			} else {
				if hookErr := r.hooks.AfterMockInsert(ctx, &MockContext{
					Mock: mock, TestSetID: newTestSetID,
				}); hookErr != nil {
					r.logger.Error("AfterMockInsert hook failed; mock was inserted successfully but post-insert hook side-effects may be missing. Check your RecordHooks implementation.",
						zap.Error(hookErr),
						zap.String("testSetID", newTestSetID),
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()))
				}
				if tempID != "" && mock.Name != "" {
					correlationMap.Store(tempID, models.MockEntry{
						Name:             mock.Name,
						Kind:             string(mock.GetKind()),
						Timestamp:        mock.Spec.ReqTimestampMock.Unix(),
						ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
						ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
					})
				}
				mockCountMapMu.Lock()
				mockCountMap[mock.GetKind()]++
				mockCountMapMu.Unlock()
				r.telemetry.RecordedTestCaseMock(mock.GetKind())
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		for mapping := range frames.Mappings {
			realMockEntries := r.resolveMappingEntries(mapping, &correlationMap, &asyncMockIDs)

			// Write to mappings.yaml
			if len(realMockEntries) > 0 {
				err := r.mappingDb.Upsert(ctx, newTestSetID, mapping.TestName, realMockEntries)
				if err != nil {
					utils.LogError(r.logger, err, "failed to save mapping")
				}
			}
		}
		return nil
	})

	if r.config.CommandType != string(utils.DockerCompose) {
		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			if runAppError.AppErrorType == models.ErrCtxCanceled {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})
	}

	// setting a timer for recording
	if r.config.Record.RecordTimer != 0 {
		errGrp.Go(func() error {
			r.logger.Info("Setting a timer of " + r.config.Record.RecordTimer.String() + " for recording")
			timer := time.After(r.config.Record.RecordTimer)
			select {
			case <-timer:
				r.logger.Info("Time up! Stopping keploy")
				err := utils.Stop(r.logger, "Time up! Stopping keploy")
				if err != nil {
					utils.LogError(r.logger, err, "failed to stop recording")
					return errors.New("failed to stop recording")
				}
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}

	// Waiting for the error to occur in any of the go routines
	select {
	case appErr := <-appErrChan:
		switch appErr.AppErrorType {
		case models.ErrCommandError:
			stopReason = "error in running the user application, hence stopping keploy"
		case models.ErrUnExpected:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
		case models.ErrInternal:
			stopReason = "internal error occurred while hooking into the application, hence stopping keploy"
		case models.ErrAppStopped:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
			r.logger.Info(stopReason, zap.Error(appErr))
			return nil
		case models.ErrCtxCanceled:
			return nil
		case models.ErrTestBinStopped:
			stopReason = "keploy test mode binary stopped, hence stopping keploy"
			return nil
		default:
			stopReason = "unknown error received from application, hence stopping keploy"
		}

	case err = <-insertTestErrChan:
		stopReason = "error while inserting test case into db, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf("%s", stopReason)
}

func (r *Recorder) GetTestAndMockChans(ctx context.Context) (FrameChan, error) {

	incomingOpts := models.IncomingOptions{
		Filters: r.config.Record.Filters,
	}

	// Create channels to receive incoming and outgoing data
	incomingChan := make(chan *models.TestCase)
	outgoingChan := make(chan *models.Mock)
	mappingChan := make(chan models.TestMockMapping)

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return FrameChan{}, fmt.Errorf("failed to get error group from context")
	}

	// INCOMING
	incomingStream, err := r.instrumentation.GetIncoming(ctx, incomingOpts)
	if err != nil {
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting incoming test cases")
			// Close channels to prevent callers from hanging when ranging over them
			close(incomingChan)
			close(outgoingChan)
			return FrameChan{Incoming: incomingChan, Outgoing: outgoingChan}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get incoming test cases: %w", err)
	}

	g.Go(func() error {
		defer close(incomingChan)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case tc, ok := <-incomingStream:
				if !ok {
					return nil
				}
				// forward but remain cancelable
				select {
				case <-ctx.Done():
					return ctx.Err()
				case incomingChan <- tc:
				}
			}
		}
	})

	// OUTGOING
	// Create a cancelable child that we always cancel when ctx is done.
	mockCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var tlsPrivateKey string
	if r.config.Record.TLSPrivateKeyPath != "" {
		keyBytes, err := os.ReadFile(r.config.Record.TLSPrivateKeyPath)
		if err != nil {
			r.logger.Error("failed to read tls private key", zap.Error(err))
			cancel()
			return FrameChan{}, err
		}
		tlsPrivateKey = string(keyBytes)
	}

	outgoingStream, err := r.instrumentation.GetOutgoing(mockCtx, models.OutgoingOptions{
		Rules:                     r.config.BypassRules,
		MongoPassword:             r.config.Test.MongoPassword,
		TLSPrivateKey:             tlsPrivateKey,
		CapturePackets:            r.config.Record.CapturePackets,
		OpportunisticTLSIntercept: r.config.Record.OpportunisticTLSIntercept,
		MysqlPorts:                r.config.MysqlPorts,
		// Advertise that this CLI understands the reserved Kind=RevokedTests
		// control frame: it diverts such a frame into a revoke set and deletes
		// those deferred-orphan test cases at finalize instead of persisting it
		// as a mock. The agent emits revoke frames ONLY when this is true.
		SupportsDroppedRevoke: true,
	})
	if err != nil {

		cancel()
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting outgoing mocks")
			// Close outgoingChan to prevent callers from hanging
			// Note: incomingChan will be closed by the goroutine started above when ctx is done
			close(outgoingChan)
			return FrameChan{Incoming: incomingChan, Outgoing: outgoingChan}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get outgoing mocks: %w", err)
	}
	g.Go(func() error {
		defer close(outgoingChan)
		defer cancel()

		// Also cancel mockCtx when parent ctx is done
		// This is done inside the goroutine to avoid goroutine leaks
		go func() {
			<-ctx.Done()
			cancel()
		}()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case m, ok := <-outgoingStream:
				if !ok {
					return nil
				}
				select {
				case <-ctx.Done():
					outgoingChan <- m
					return ctx.Err()
				case outgoingChan <- m:
				}
			}
		}
	})

	// MAPPINGS
	g.Go(func() error {
		defer close(mappingChan)

		// Create context that cancels with parent
		mapCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		go func() {
			<-ctx.Done()
			cancel()
		}()

		// Call the new AgentClient method
		ch, err := r.instrumentation.GetMappings(mapCtx, incomingOpts)
		if err != nil {
			if ctx.Err() != nil || utils.IsShutdownError(err) {
				return nil
			}
			return fmt.Errorf("failed to get mappings: %w", err)
		}

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case m, ok := <-ch:
				if !ok {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case mappingChan <- m:
				}
			}
		}
	})

	return FrameChan{
		Incoming: incomingChan,
		Outgoing: outgoingChan,
		Mappings: mappingChan,
	}, nil

}

func (r *Recorder) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, opts)
}

func (r *Recorder) GetNextTestSetID(ctx context.Context) (string, error) {
	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get test set IDs: %w", err)
	}

	if r.config.Record.Metadata == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}
	r.config.Record.Metadata = utils.TrimSpaces(r.config.Record.Metadata)
	meta, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil || meta == nil {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	nameVal, ok := meta["name"]
	requestedName, isStr := nameVal.(string)
	if !ok || !isStr || requestedName == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	existingIDs := make(map[string]struct{}, len(testSetIDs))
	for _, id := range testSetIDs {
		existingIDs[id] = struct{}{}
	}

	if _, occupied := existingIDs[requestedName]; !occupied {
		return requestedName, nil
	}

	var highestSuffix int
	namePrefix := requestedName + "-"
	for id := range existingIDs {
		if !strings.HasPrefix(id, namePrefix) {
			continue
		}
		suffixPart := id[len(namePrefix):]
		if n, err := strconv.Atoi(suffixPart); err == nil && n > highestSuffix {
			highestSuffix = n
		}
	}

	newSuffix := highestSuffix + 1
	assignedName := fmt.Sprintf("%s-%d", requestedName, newSuffix)

	r.logger.Info(fmt.Sprintf(
		"Test set name '%s' already exists, using '%s' instead. You can change this name if you want.",
		requestedName, assignedName,
	))

	return assignedName, nil
}

func (r *Recorder) createConfigWithMetadata(ctx context.Context, testSetID string) {
	// Parse metadata from the config
	metadata, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil {
		utils.LogError(r.logger, err, "failed to parse metadata", zap.String("metadata", r.config.Record.Metadata))
		return
	}
	testSet := &models.TestSet{
		PreScript:  "",
		PostScript: "",
		Template:   make(map[string]interface{}),
		Metadata:   metadata,
	}

	err = r.testSetConf.Write(ctx, testSetID, testSet)
	if err != nil {
		utils.LogError(r.logger, err, "Failed to create test-set config file with metadata", zap.String("testSet", testSetID))
		return
	}

	r.logger.Info("Created test-set config file with metadata")
}

// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

const (
	// mappingDrainGrace hard-caps how long recording shutdown waits for the agent
	// to flush the test<->mock mappings it has already resolved. Bounded so a
	// wedged agent cannot hang exit, and kept under the 30s DrainErrGroup budget
	// that Start's teardown gives the group this drain runs in — overshooting that
	// would trade a lost tail for a teardown timeout, which is no better.
	mappingDrainGrace = 15 * time.Second

	// mappingFlushBatch is how many mappings accumulate before mappings.yaml is
	// rewritten. Each rewrite re-encodes the whole file, so writing per mapping is
	// quadratic (368 tests: 164us for the first, 19.45ms for the last, ~2.7s of
	// pure rewriting) — slow enough to back-pressure the agent's mapping stream,
	// which then DROPS what it cannot hand over. Batching keeps the consumer far
	// ahead of the stream.
	mappingFlushBatch = 32

	// mappingFlushInterval bounds how long a partial batch waits, so a long
	// recording still persists mappings as it goes instead of holding them all in
	// memory until the stream closes.
	mappingFlushInterval = 2 * time.Second

	// mappingIdleGrace ends the shutdown drain once the mapping stream falls idle.
	// The agent holds the stream open for the whole session, so idleness — not EOF
	// — is what signals the tail is through. Sized well above the agent's
	// per-mapping flush latency so a slow flush is not mistaken for completion.
	mappingIdleGrace = 3 * time.Second
)

// stopTimer disarms t, draining its channel if it had already fired, so a later
// Reset starts from a clean state.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// resetTimer re-arms t for d, safely draining any pending fire first.
func resetTimer(t *time.Timer, d time.Duration) {
	stopTimer(t)
	t.Reset(d)
}

// mergeMockEntries unions incoming entries into existing ones by mock name,
// preserving recorded order. Mirrors the mapping store's merge: a test's mapping
// arrives in more than one piece and the later pieces are deltas, so replacing
// would delete mocks the earlier piece recorded.
func mergeMockEntries(existing, incoming []models.MockEntry) []models.MockEntry {
	if len(existing) == 0 {
		return incoming
	}
	seen := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		seen[e.Name] = struct{}{}
	}
	merged := existing
	for _, e := range incoming {
		if _, dup := seen[e.Name]; dup {
			continue
		}
		seen[e.Name] = struct{}{}
		merged = append(merged, e)
	}
	return merged
}

// consumeMappings correlates each test<->mock mapping the agent streams to its
// real mock entries and persists it to mappings.yaml. It runs until mappings is
// closed by the producer, and returns only then — ctx bounds nothing here by
// design.
//
// Mappings are the last artifact a recorded test produces (the agent resolves a
// test's mock range only once that test is done), so the tail of every endpoint
// arrives while recording is already shutting down. Writes therefore run on a
// context detached from ctx: on a cancelled one the yaml store refuses every
// write and those tests vanish from mappings.yaml, which replay then reports as
// no_mocks. Detaching here rather than relying on the caller keeps that
// guarantee local to the loop that depends on it — see the persistCtx note in
// Start for the same requirement on the test-case and mock stores.
func (r *Recorder) consumeMappings(ctx context.Context, testSetID string, mappings <-chan models.TestMockMapping, correlationMap, asyncMockIDs *sync.Map) error {
	persistCtx := context.WithoutCancel(ctx)

	// pending batches mappings so the file is rewritten once per batch instead of
	// once per test. flushMu guards it because the ticker below flushes too.
	pending := make(map[string][]models.MockEntry, mappingFlushBatch)
	var flushMu sync.Mutex

	flush := func() {
		flushMu.Lock()
		defer flushMu.Unlock()
		if len(pending) == 0 {
			return
		}
		if err := r.mappingDb.UpsertBatch(persistCtx, testSetID, pending); err != nil {
			// Deliberately not utils.LogError: it suppresses context.Canceled,
			// which is exactly the class this write used to fail with, so a lost
			// batch left no trace at all. A mapping that goes missing here is a
			// no_mocks failure at replay — it must never be silent again.
			names := make([]string, 0, len(pending))
			for tn := range pending {
				names = append(names, tn)
			}
			sort.Strings(names)
			r.logger.Error("failed to save mappings",
				zap.Strings("tests", names),
				zap.Error(err),
				zap.String("next_step", "these tests' mocks will be missing from mappings.yaml and replay will report no_mocks for them; re-record the test set"))
		}
		clear(pending)
	}

	// A partial batch must not sit in memory until the stream closes: recording
	// can run for hours, and an operator watching mappings.yaml should see it
	// grow. The ticker bounds how long a mapping stays unpersisted without
	// putting a file rewrite on the per-mapping path.
	ticker := time.NewTicker(mappingFlushInterval)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				flush()
			}
		}
	}()

	for mapping := range mappings {
		realMockEntries := r.resolveMappingEntries(mapping, correlationMap, asyncMockIDs)

		if len(realMockEntries) == 0 {
			continue
		}

		flushMu.Lock()
		// Union, don't replace: the agent emits a test's mocks when its window
		// resolves and emits more later for mocks retroactively binned into that
		// window. Those later emissions are a DELTA — overwriting would delete the
		// mocks already recorded for the test and replay it with a short pool.
		pending[mapping.TestName] = mergeMockEntries(pending[mapping.TestName], realMockEntries)
		full := len(pending) >= mappingFlushBatch
		flushMu.Unlock()

		// Flush on size only. Draining the channel is the priority: every
		// millisecond spent rewriting mappings.yaml is a millisecond the agent
		// cannot hand over its next mapping, and it DROPS what it cannot hand
		// over. The ticker flushes whatever a partial batch leaves behind.
		if full {
			flush()
		}
	}

	// The stream is closed: persist whatever the last partial batch holds.
	flush()
	return nil
}

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

	// persistCtx is the context the test-case and mock stores are written on,
	// detached from ctx on purpose. (consumeMappings makes the same guarantee for
	// mappings.yaml itself.)
	//
	// Capture and persistence are driven by different contexts, and the gap
	// between them is where recorded data used to disappear. The agent streams on
	// reqCtx (see below), which is WithoutCancel'd and so survives SIGINT — it
	// stops only at reqCtxCancel() in teardown, AFTER the user app has been
	// stopped and drained. The consumer goroutines below, however, are children of
	// ctx, which cancels the instant SIGINT lands. So for the whole teardown
	// window (a NotifyGracefulShutdown of up to 10s, then an app drain of up to
	// 30s) the agent kept handing us test cases, mocks and mappings while every
	// store refused to write them: the yaml path returns ctx.Err() on a cancelled
	// ctx, and the callers below treated that as "shutting down, skip" — silently.
	//
	// The damage was worst for mappings, which are emitted LAST (the agent can
	// only resolve a test's mock range once that test is done), so the tail of
	// every endpoint landed squarely in that window: mappings.yaml lost those
	// tests and replay reported "no_mocks" (candidates: 0) for exactly them. A
	// dropped mock insert compounded it by also skipping its correlationMap entry,
	// stranding the mapping that referenced it even when the mapping did arrive.
	//
	// Writes must therefore outlive cancellation. This does NOT risk a hung
	// teardown: the consumers are `range` loops that end when the agent closes its
	// streams (at reqCtxCancel), each write is a local file op, and Start's
	// teardown still bounds the whole group with DrainErrGroup.
	persistCtx := context.WithoutCancel(ctx)

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
			err := r.testDB.InsertTestCase(persistCtx, testCase, newTestSetID, true)
			if err != nil {
				if ctx.Err() != nil {
					// Once shutdown has begun nothing reads insertTestErrChan —
					// Start's select has returned and the channel is closed on
					// Start's way out — so reporting through it is unsafe. Log
					// instead of dropping the error on the floor: with persistCtx
					// the write no longer fails merely because we are shutting
					// down, so an error here is real and the operator needs it.
					utils.LogError(r.logger, err, "failed to record test case during shutdown",
						zap.String("testCaseName", testCase.Name),
						zap.String("testSetID", newTestSetID))
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
			mockCtx := &MockContext{Mock: mock, TestSetID: newTestSetID}
			if hookErr := r.hooks.BeforeMockInsert(ctx, mockCtx); hookErr != nil {
				r.logger.Error("BeforeMockInsert hook failed; recording will continue but hook side-effects may be missing. Check your RecordHooks implementation.",
					zap.Error(hookErr),
					zap.String("testSetID", newTestSetID),
					zap.String("mockName", mock.Name),
					zap.String("mockKind", mock.GetKind()))
			}
			if mockCtx.Skip {
				// A hook asked to drop this mock (collapsed async poll no-change
				// cycle). Do not persist, map, or correlate it.
				continue
			}
			// The AsyncRecorder hook sets Spec.Async in BeforeMockInsert above;
			// remember it so the mapping goroutine below never per-test maps it.
			if mock.IsAsync() {
				asyncMockIDs.Store(tempID, struct{}{})
			}
			err := r.mockDB.InsertMock(persistCtx, mock, newTestSetID)
			if err != nil {
				if ctx.Err() != nil {
					// See the sibling note on the test-case insert: insertMockErrChan
					// is no longer safe to report through once teardown has begun. A
					// skipped mock also skips its correlationMap entry below, which
					// strands the mapping that references it — so this must be loud.
					utils.LogError(r.logger, err, "failed to record mock during shutdown",
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()),
						zap.String("testSetID", newTestSetID))
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
		return r.consumeMappings(ctx, newTestSetID, frames.Mappings, &correlationMap, &asyncMockIDs)
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

		// A mapping is the LAST artifact a recorded test produces: it is derived,
		// and the agent can only emit it once that test's mock range has resolved.
		// So when recording stops, a burst for the most recently recorded tests is
		// still queued inside the agent. Tearing the stream down the instant ctx was
		// done dropped that burst SILENTLY — the entries never reach the consumer, so
		// not even a write error is logged. mappings.yaml then omitted the tail of
		// every endpoint, and replay reported "no_mocks" (candidates: 0) for exactly
		// those tests, because resolveMockSets picks mapping-based selection per test
		// SET and has no per-test fallback for a test that is missing.
		//
		// It is tempting to assume the tail has already been flushed by now because
		// teardown stops the app first. That is only true for the NATIVE path: under
		// docker-compose keploy never runs the app at all (see the CommandType check
		// above), runAppErrGrp is empty, its drain returns instantly, and reqCtxCancel
		// therefore fires within milliseconds of SIGINT — with the agent's queue still
		// full. Docker-compose is what most e2e lanes and many users run, and it is
		// where this bug was measured: 27 of 342 tests lost, all tails.
		//
		// So on shutdown keep the stream open and keep draining. Stop as soon as the
		// tail is through — the agent closing the stream, or the stream falling idle —
		// and hard-cap the total wait so a wedged agent cannot hang exit.
		mapCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()

		drained := make(chan struct{})
		defer close(drained)
		go func() {
			select {
			case <-drained:
				return // finished before shutdown — nothing to wait for
			case <-ctx.Done():
			}
			select {
			case <-drained: // tail fully drained
			case <-time.After(mappingDrainGrace):
				// Hitting the cap means the agent never finished flushing, so the
				// tail is truncated here — the same damage this drain prevents.
				// Say so: silence is what made the original bug so hard to find.
				r.logger.Warn("timed out draining test-mock mappings on shutdown",
					zap.Duration("waited", mappingDrainGrace),
					zap.String("next_step", "mappings.yaml may be missing the last recorded tests and replay can report no_mocks for them; re-record the test set and report this if it recurs"))
			}
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

		// idle is armed only once shutdown has begun. While recording is live, gaps
		// between mappings are normal and must never end the stream; once shutdown
		// starts, a gap long enough means the agent has nothing left to flush (it
		// holds the stream open for the whole session, so it will not close it).
		idle := time.NewTimer(mappingIdleGrace)
		defer idle.Stop()
		stopTimer(idle)

		for {
			// While recording is live, wake on ctx.Done(); once draining, wait on the
			// idle timer instead. Arming both would let an already-closed ctx.Done()
			// spin the loop.
			var idleC <-chan time.Time
			var shutdownC <-chan struct{}
			if ctx.Err() != nil {
				resetTimer(idle, mappingIdleGrace)
				idleC = idle.C
			} else {
				shutdownC = ctx.Done()
			}

			select {
			case <-shutdownC:
				// Shutdown began: re-loop to arm the idle timer and start draining.
				continue
			case <-mapCtx.Done():
				// Hard cap reached, or the stream was torn down.
				return nil
			case <-idleC:
				// Draining and nothing for mappingIdleGrace: the tail is through.
				return nil
			case m, ok := <-ch:
				if !ok {
					return nil
				}
				select {
				case <-mapCtx.Done():
					// Hand off the mapping we already took off the stream before
					// unwinding — cancellation is not a licence to drop data we are
					// holding. Both cases are ready once mapCtx is done, so the
					// runtime picks at random and this would otherwise lose the last
					// in-flight mapping about half the time. The consumer is still
					// ranging (it stops only when this goroutine closes the channel
					// on return), so the send completes. Mirrors the outgoing
					// producer above.
					mappingChan <- m
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

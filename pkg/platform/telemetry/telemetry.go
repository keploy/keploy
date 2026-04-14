// Package telemetry collects anonymous usage metrics.
package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var teleURL = "https://telemetry.keploy.io/analytics"

const (
	// telemetryHTTPTimeout bounds each POST to the analytics endpoint. Used
	// as the http.Client timeout and as the basis for the Shutdown drain
	// deadline so the two can't drift out of sync.
	telemetryHTTPTimeout = 2 * time.Second
	// telemetryShutdownGrace is extra headroom beyond telemetryHTTPTimeout
	// for Shutdown's inflight.Wait — guarantees an in-flight POST can
	// complete or time out cleanly before Shutdown returns.
	telemetryShutdownGrace = 500 * time.Millisecond
	// flushInterval caps how long batched recording counters can sit in
	// memory before being emitted. Shorter than the original 30s to shrink
	// the crash-loss window under SIGKILL/OOM while still keeping network
	// traffic constant (~6 req/min worst case).
	flushInterval = 10 * time.Second
	// flushThreshold bounds crash loss by volume: once pending recorded
	// events reach this many, the flush loop is kicked even if the ticker
	// hasn't fired yet.
	flushThreshold = 500
)

type Telemetry struct {
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	KeployVersion  string
	GlobalMap      *sync.Map
	client         *http.Client
	mu             sync.Mutex // guards closed + inflight.Add to prevent Add/Wait race
	inflight       sync.WaitGroup
	inflightN      atomic.Int64
	closed         atomic.Bool

	// Batched recording state. recordingMu serializes hot-path increments
	// with the flush goroutine's swap and with Shutdown's final drain, so
	// the closed-gate + double-checked-locking pattern in the hot path
	// cannot lose events during Shutdown.
	recordingMu sync.Mutex
	// recordedTests is the pending test count for the current flush epoch.
	// Authoritative — reset to 0 on flush. Atomic so maybeKickFlush can
	// read it without taking recordingMu.
	recordedTests atomic.Int64
	// mocksByType preserves the per-protocol breakdown that the legacy
	// per-event RecordedTestCaseMock payload carried in meta.mock.
	// Authoritative source for the flush payload.
	mocksByType map[string]int64
	// mockKickCounter shadows len(mocksByType) as a lock-free hint for
	// maybeKickFlush. Not authoritative — the flush payload reads from
	// mocksByType. Kept loosely in sync under recordingMu.
	mockKickCounter  atomic.Int64
	flushOnce        sync.Once
	flushLoopStarted atomic.Bool   // true once the flush goroutine has launched
	flushDone        chan struct{} // closed when flush loop exits; initialized in NewTelemetry
	flushKick        chan struct{} // buffered(1); non-blocking kick from hot path on threshold
	closeCh          chan struct{} // signals the flush loop to stop
}

type Options struct {
	Enabled        bool
	Version        string
	GlobalMap      *sync.Map
	InstallationID string
}

func NewTelemetry(logger *zap.Logger, opt Options) *Telemetry {
	gm := opt.GlobalMap
	if gm == nil {
		gm = &sync.Map{}
	}
	return &Telemetry{
		Enabled:        opt.Enabled,
		logger:         logger,
		KeployVersion:  opt.Version,
		GlobalMap:      gm,
		InstallationID: opt.InstallationID,
		client:         &http.Client{Timeout: telemetryHTTPTimeout},
		mocksByType:    make(map[string]int64),
		flushDone:      make(chan struct{}),
		flushKick:      make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
	}
}

func (tel *Telemetry) Ping(ctx context.Context) {
	if !tel.Enabled {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		select {
		case <-ctx.Done():
			return
		default:
			if tel.closed.Load() {
				return
			}
			tel.SendTelemetry("Ping")
		}
		for {
			if tel.closed.Load() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tel.SendTelemetry("Ping")
			}
		}
	}()
}

func (tel *Telemetry) TestSetRun(success int, failure int, testSet string, runStatus string) {
	dataMap := map[string]interface{}{
		"Passed-Tests": success,
		"Failed-Tests": failure,
		"Test-Set":     testSet,
		"Run-Status":   runStatus,
	}
	tel.sendTracked("TestSetRun", dataMap)
}

func (tel *Telemetry) TestRun(success int, failure int, testSets int, runStatus string, metadata map[string]interface{}) {
	dataMap := map[string]interface{}{
		"Passed-Tests": success,
		"Failed-Tests": failure,
		"Test-Sets":    testSets,
		"Run-Status":   runStatus,
	}
	for k, v := range metadata {
		dataMap[k] = v
	}
	tel.sendTracked("TestRun", dataMap)
}

func (tel *Telemetry) MockTestRun(utilizedMocks int) {
	dataMap := map[string]interface{}{
		"Utilized-Mocks": utilizedMocks,
	}
	tel.sendTracked("MockTestRun", dataMap)
}

func (tel *Telemetry) RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int, metadata map[string]interface{}) {
	mockMap := make(map[string]interface{}, len(mockTotal))
	for k, v := range mockTotal {
		mockMap[k] = v
	}
	dataMap := map[string]interface{}{
		"test-set": testSet,
		"tests":    testsTotal,
		"mocks":    mockMap,
	}
	for k, v := range metadata {
		dataMap[k] = v
	}
	tel.sendTracked("RecordedTestSuite", dataMap)
}

func (tel *Telemetry) RecordedTestAndMocks() {
	if !tel.Enabled || tel.closed.Load() {
		return
	}
	tel.recordingMu.Lock()
	// Double-checked locking against Shutdown: if closed was flipped
	// while we were waiting on the mutex, bail without mutating state so
	// Shutdown's snapshot is authoritative.
	if tel.closed.Load() {
		tel.recordingMu.Unlock()
		return
	}
	tel.recordedTests.Add(1)
	// ensureFlushLoop must run under recordingMu so the goroutine cannot
	// be spawned after Shutdown has already completed phase 3; otherwise
	// the loop would leak with nothing left to signal it.
	tel.ensureFlushLoop()
	tel.recordingMu.Unlock()
	tel.maybeKickFlush()
}

func (tel *Telemetry) GenerateUT() {
	tel.SendTelemetry("GenerateUT")
}

func (tel *Telemetry) RecordedMocks(mockTotal map[string]int) {
	mockMap := make(map[string]interface{}, len(mockTotal))
	for k, v := range mockTotal {
		mockMap[k] = v
	}
	dataMap := map[string]interface{}{
		"mocks": mockMap,
	}
	tel.SendTelemetry("RecordedMocks", dataMap)
}

func (tel *Telemetry) RecordedTestCaseMock(mockType string) {
	if !tel.Enabled || tel.closed.Load() {
		return
	}
	tel.recordingMu.Lock()
	// Double-checked locking against Shutdown; see RecordedTestAndMocks.
	if tel.closed.Load() {
		tel.recordingMu.Unlock()
		return
	}
	tel.mocksByType[mockType]++
	tel.mockKickCounter.Add(1)
	// ensureFlushLoop must run under recordingMu; see RecordedTestAndMocks.
	tel.ensureFlushLoop()
	tel.recordingMu.Unlock()
	tel.maybeKickFlush()
}

// maybeKickFlush fires a non-blocking signal to the flush loop when the
// combined pending event count has reached flushThreshold. The kick
// channel is buffered(1), so multiple rapid callers collapse into a single
// pending wake-up and the hot path never blocks.
func (tel *Telemetry) maybeKickFlush() {
	if tel.recordedTests.Load()+tel.mockKickCounter.Load() < flushThreshold {
		return
	}
	select {
	case tel.flushKick <- struct{}{}:
	default:
	}
}

// ensureFlushLoop starts a single background goroutine that periodically
// flushes batched recording counters. This replaces the per-event goroutine
// pattern that previously spawned thousands of goroutines under load.
//
// MUST be called with recordingMu held. The mutex serializes the
// flushLoopStarted store with Shutdown's closed flip so the goroutine
// cannot be spawned after Shutdown has already skipped the close(closeCh)
// path — otherwise the loop would leak with nothing left to signal it.
func (tel *Telemetry) ensureFlushLoop() {
	tel.flushOnce.Do(func() {
		tel.flushLoopStarted.Store(true)
		go func() {
			defer close(tel.flushDone)
			ticker := time.NewTicker(flushInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					tel.flushRecordingCounters()
				case <-tel.flushKick:
					tel.flushRecordingCounters()
				case <-tel.closeCh:
					// Final drain is performed inline by Shutdown under
					// recordingMu + closed=true so nothing can race it.
					// The loop just exits here.
					return
				}
			}
		}()
	})
}

// flushRecordingCounters snapshots and resets the batched counters under
// recordingMu, then emits a RecordedBatch event. The mocks_by_type map
// preserves the dimension the legacy per-event RecordedTestCaseMock
// payload carried in meta.mock.
//
// The send always goes through sendFinal, which bypasses sendEvent's
// closed gate. The closed gate exists to reject new user-initiated
// events (TestRun, TestSetRun, MockTestRun, …) after Shutdown signals
// shutdown. Flush sends are not new events — they are drains of state
// the hot path already committed into mocksByType, and dropping a drain
// is losing already-promised data. Using sendFinal also closes the
// race where the flush loop releases recordingMu, Shutdown phase 1
// flips closed, and the loop's subsequent send would otherwise bail at
// the gate.
func (tel *Telemetry) flushRecordingCounters() {
	tel.recordingMu.Lock()
	tests := tel.recordedTests.Swap(0)
	pending := tel.mocksByType
	tel.mocksByType = make(map[string]int64)
	tel.mockKickCounter.Store(0)
	tel.recordingMu.Unlock()

	var totalMocks int64
	for _, v := range pending {
		totalMocks += v
	}

	if tests == 0 && totalMocks == 0 {
		return
	}

	dataMap := map[string]interface{}{
		"recorded_tests": tests,
		"recorded_mocks": totalMocks,
	}
	if len(pending) > 0 {
		byType := make(map[string]interface{}, len(pending))
		for k, v := range pending {
			byType[k] = v
		}
		dataMap["mocks_by_type"] = byType
	}
	tel.sendFinal("RecordedBatch", dataMap)
}

func (tel *Telemetry) SendTelemetry(eventType string, output ...map[string]interface{}) {
	tel.sendEvent(eventType, false, false, output...)
}

func (tel *Telemetry) sendTracked(eventType string, output ...map[string]interface{}) {
	tel.sendEvent(eventType, true, false, output...)
}

// sendFinal is the post-closed counterpart to sendTracked. It bypasses
// the closed gate in sendEvent so Shutdown's final drain actually reaches
// the server, while still registering the POST with inflight so the
// drain wait covers it. Must only be called from Shutdown after closed
// has been set.
func (tel *Telemetry) sendFinal(eventType string, output ...map[string]interface{}) {
	tel.sendEvent(eventType, true, true, output...)
}

func (tel *Telemetry) sendEvent(eventType string, tracked bool, force bool, output ...map[string]interface{}) {
	if !tel.Enabled {
		return
	}

	if tracked {
		tel.mu.Lock()
		if !force && tel.closed.Load() {
			tel.mu.Unlock()
			return
		}
		tel.inflight.Add(1)
		tel.inflightN.Add(1)
		tel.mu.Unlock()
	} else if tel.closed.Load() {
		return
	}

	event := models.TeleEvent{
		EventType: eventType,
		CreatedAt: time.Now().Unix(),
	}
	if len(output) > 0 && output[0] != nil {
		event.Meta = output[0]
	} else {
		event.Meta = map[string]interface{}{}
	}

	tel.GlobalMap.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			event.Meta[k] = value
		}
		return true
	})

	event.InstallationID = tel.InstallationID
	event.OS = runtime.GOOS
	event.KeployVersion = tel.KeployVersion
	event.Arch = runtime.GOARCH

	go func() {
		if tracked {
			defer func() {
				tel.inflightN.Add(-1)
				tel.inflight.Done()
			}()
		}

		func() {
			defer func() { _ = recover() }()
			event.IsCI, event.CIProvider = detectCI()
			event.GitRepo = detectGitRepo()
		}()

		bin, err := marshalEvent(event)
		if err != nil {
			return
		}

		req, err := http.NewRequest(http.MethodPost, teleURL, bytes.NewBuffer(bin))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, err := tel.client.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}

func (tel *Telemetry) Shutdown() {
	if !tel.Enabled {
		return
	}

	// Phase 1: flip closed under recordingMu + mu so every hot-path
	// caller either completes its write before we acquired the lock or
	// observes closed=true via the double-checked load and bails. This
	// is the fence that makes the final drain below race-free.
	tel.recordingMu.Lock()
	tel.mu.Lock()
	if !tel.closed.CompareAndSwap(false, true) {
		tel.mu.Unlock()
		tel.recordingMu.Unlock()
		return
	}
	tel.mu.Unlock()
	tel.recordingMu.Unlock()

	// Phase 2: inline final drain. flushRecordingCounters re-acquires
	// recordingMu — safe because every hot-path thread past its outer
	// closed check will DCL-fail under the lock after phase 1. The
	// send bypasses the closed gate via sendFinal unconditionally.
	tel.flushRecordingCounters()

	// Phase 3: stop the flush loop if it was ever started. Skipping this
	// when the loop never launched avoids a pointless grace-period wait
	// on flushDone (which would never close).
	if tel.flushLoopStarted.Load() {
		close(tel.closeCh)
		select {
		case <-tel.flushDone:
		case <-time.After(telemetryShutdownGrace):
		}
	}

	// Phase 4: wait for any in-flight HTTP POSTs (including the one we
	// just kicked off in phase 2).
	if tel.inflightN.Load() == 0 {
		return
	}
	if tel.logger != nil {
		tel.logger.Info("Cleaning up running operations...")
	}
	done := make(chan struct{})
	go func() {
		tel.inflight.Wait()
		close(done)
	}()
	// Wait long enough for an in-flight POST to either complete or hit
	// its own HTTP-client timeout. telemetryShutdownGrace provides
	// headroom so the two deadlines can't race.
	select {
	case <-done:
	case <-time.After(telemetryHTTPTimeout + telemetryShutdownGrace):
	}
}

package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// receivedBatch captures the fields of models.TeleEvent the tests care about.
// We decode loosely so tests don't break on unrelated schema changes.
type receivedBatch struct {
	EventType string                 `json:"eventType"`
	Meta      map[string]interface{} `json:"meta"`
}

// recorder is a test double for the keploy-telemetry HTTP endpoint. It
// captures every POST body and exposes thread-safe accessors.
type recorder struct {
	mu      sync.Mutex
	batches []receivedBatch
}

func (r *recorder) handler(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var b receivedBatch
	_ = json.Unmarshal(body, &b)
	r.mu.Lock()
	r.batches = append(r.batches, b)
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *recorder) snapshot() []receivedBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]receivedBatch, len(r.batches))
	copy(out, r.batches)
	return out
}

// newTestTelemetry starts a test HTTP server, points teleURL at it, and
// returns a Telemetry wired to send there. The server is torn down and
// teleURL is restored via t.Cleanup.
func newTestTelemetry(t *testing.T) (*Telemetry, *recorder) {
	t.Helper()
	r := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	t.Cleanup(srv.Close)

	origURL := teleURL
	teleURL = srv.URL
	t.Cleanup(func() { teleURL = origURL })

	tel := NewTelemetry(zap.NewNop(), Options{
		Enabled:        true,
		Version:        "test",
		InstallationID: "test-installation",
	})
	return tel, r
}

// sumBatches reduces a set of received batches into the fields the tests
// assert against. Iterating in the test itself would duplicate the same
// float64 type assertions across every case.
func sumBatches(batches []receivedBatch) (tests, mocks int64, byType map[string]int64) {
	byType = make(map[string]int64)
	for _, b := range batches {
		if b.EventType != "RecordedBatch" {
			continue
		}
		if v, ok := b.Meta["recorded_tests"].(float64); ok {
			tests += int64(v)
		}
		if v, ok := b.Meta["recorded_mocks"].(float64); ok {
			mocks += int64(v)
		}
		if m, ok := b.Meta["mocks_by_type"].(map[string]interface{}); ok {
			for k, v := range m {
				if f, ok := v.(float64); ok {
					byType[k] += int64(f)
				}
			}
		}
	}
	return tests, mocks, byType
}

// TestFlushRecordingCountersPayload verifies the basic RecordedBatch
// shape: event_type, summed counts, and per-protocol breakdown in
// mocks_by_type.
func TestFlushRecordingCountersPayload(t *testing.T) {
	tel, rec := newTestTelemetry(t)

	tel.RecordedTestAndMocks()
	tel.RecordedTestAndMocks()
	tel.RecordedTestCaseMock("http")
	tel.RecordedTestCaseMock("http")
	tel.RecordedTestCaseMock("grpc")

	tel.Shutdown()

	batches := rec.snapshot()
	if len(batches) != 1 {
		t.Fatalf("want 1 batch, got %d: %+v", len(batches), batches)
	}
	if batches[0].EventType != "RecordedBatch" {
		t.Errorf("eventType: want RecordedBatch, got %q", batches[0].EventType)
	}
	tests, mocks, byType := sumBatches(batches)
	if tests != 2 {
		t.Errorf("recorded_tests: want 2, got %d", tests)
	}
	if mocks != 3 {
		t.Errorf("recorded_mocks: want 3, got %d", mocks)
	}
	if byType["http"] != 2 {
		t.Errorf("mocks_by_type[http]: want 2, got %d", byType["http"])
	}
	if byType["grpc"] != 1 {
		t.Errorf("mocks_by_type[grpc]: want 1, got %d", byType["grpc"])
	}
}

// TestThresholdKickFlushesEarly verifies the volume-based kick: once
// pending events cross flushThreshold, the flush loop is woken without
// waiting for the 10s ticker. Without this behavior the test would have
// to wait flushInterval for the first batch and would fail its polling
// deadline.
func TestThresholdKickFlushesEarly(t *testing.T) {
	tel, rec := newTestTelemetry(t)
	defer tel.Shutdown()

	for i := 0; i < flushThreshold; i++ {
		tel.RecordedTestCaseMock("http")
	}

	// Wait for the kick → flush → HTTP round-trip. 2s is far shorter
	// than flushInterval (10s), so a pass here can only come from the
	// threshold kick path, not the ticker.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	batches := rec.snapshot()
	if len(batches) == 0 {
		t.Fatal("threshold kick did not trigger an early flush within 2s")
	}
	_, mocks, byType := sumBatches(batches)
	if mocks == 0 {
		t.Errorf("expected recorded_mocks > 0 in kicked batch, got 0")
	}
	if byType["http"] == 0 {
		t.Errorf("expected mocks_by_type[http] > 0 in kicked batch, got 0")
	}
}

// TestShutdownNoLossUnderConcurrentRecording is the regression test for
// the Shutdown-race bug: events recorded before Shutdown is called must
// all end up in the received batches, even when the hot path is driven
// from multiple goroutines. Run under -race this also catches any
// mutation of mocksByType without holding recordingMu.
func TestShutdownNoLossUnderConcurrentRecording(t *testing.T) {
	tel, rec := newTestTelemetry(t)

	const workers = 10
	const perWorker = 500 // 10 * 500 = 5000, comfortably above flushThreshold

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				tel.RecordedTestCaseMock("http")
				tel.RecordedTestAndMocks()
			}
		}()
	}
	wg.Wait()

	tel.Shutdown()

	want := int64(workers * perWorker)
	tests, mocks, byType := sumBatches(rec.snapshot())
	if tests != want {
		t.Errorf("recorded_tests: want %d, got %d (loss: %d)", want, tests, want-tests)
	}
	if mocks != want {
		t.Errorf("recorded_mocks: want %d, got %d (loss: %d)", want, mocks, want-mocks)
	}
	if byType["http"] != want {
		t.Errorf("mocks_by_type[http]: want %d, got %d (loss: %d)", want, byType["http"], want-byType["http"])
	}
}

// TestShutdownFastWhenNothingRecorded guards against the regression
// where Shutdown waits the full telemetryShutdownGrace on flushDone
// even when the flush loop was never started (because no recording
// happened). This is the common path for every keploy command other
// than `keploy record`.
func TestShutdownFastWhenNothingRecorded(t *testing.T) {
	tel, _ := newTestTelemetry(t)

	start := time.Now()
	tel.Shutdown()
	elapsed := time.Since(start)

	// Generous bound: the non-recording path should be ~instant. If
	// flushLoopStarted guarding breaks, this balloons to ≥500ms.
	if elapsed > 100*time.Millisecond {
		t.Errorf("Shutdown with no recording took %v, expected <100ms", elapsed)
	}
}

// TestShutdownIsIdempotent guards the CompareAndSwap gate — calling
// Shutdown twice should not panic, double-close closeCh, or re-flush.
func TestShutdownIsIdempotent(t *testing.T) {
	tel, _ := newTestTelemetry(t)
	tel.RecordedTestAndMocks()
	tel.Shutdown()
	tel.Shutdown() // must not panic
}

// TestFlushDrainsAfterClosedFlipped is the deterministic regression test
// for the periodic-flush-vs-Shutdown race. Simulates the exact sequence:
// hot path commits data into mocksByType, then closed is flipped (as if
// Shutdown phase 1 had run), then flushRecordingCounters is invoked (as
// if the flush loop had woken on a ticker tick or kick right after the
// flip). All snapshotted data must still reach the server.
//
// Without the fix, flushRecordingCounters routes through sendTracked,
// which bails at sendEvent's closed gate and silently drops the batch.
// With the fix it routes through sendFinal, which bypasses the gate
// because drains are not new-event acceptance.
func TestFlushDrainsAfterClosedFlipped(t *testing.T) {
	tel, rec := newTestTelemetry(t)

	// The test flips closed directly below, which means Shutdown's CAS
	// gate will early-return without closing closeCh. Register a manual
	// teardown so the flush goroutine started by the first Recorded*
	// call below doesn't leak past the test. Runs before srv.Close and
	// the teleURL restore because t.Cleanup is LIFO.
	t.Cleanup(func() {
		if tel.flushLoopStarted.Load() {
			close(tel.closeCh)
			<-tel.flushDone
		}
	})

	tel.RecordedTestCaseMock("http")
	tel.RecordedTestCaseMock("http")
	tel.RecordedTestCaseMock("grpc")
	tel.RecordedTestAndMocks()

	// Flip closed out-of-band to simulate Shutdown phase 1 interposing
	// between a flush loop iteration's recordingMu.Unlock() and its
	// subsequent send.
	tel.closed.Store(true)

	// Drive the drain path directly. With the fix this reaches the
	// server; without the fix it drops at the closed gate.
	tel.flushRecordingCounters()

	// We can't call Shutdown() to wait on inflight because we've
	// already flipped closed behind its back — its CAS gate would
	// early-return. Poll the recorder directly for the HTTP round-trip.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	batches := rec.snapshot()
	if len(batches) != 1 {
		t.Fatalf("drain was dropped at closed gate: want 1 batch, got %d", len(batches))
	}
	tests, mocks, byType := sumBatches(batches)
	if tests != 1 {
		t.Errorf("recorded_tests: want 1, got %d", tests)
	}
	if mocks != 3 {
		t.Errorf("recorded_mocks: want 3, got %d", mocks)
	}
	if byType["http"] != 2 {
		t.Errorf("mocks_by_type[http]: want 2, got %d", byType["http"])
	}
	if byType["grpc"] != 1 {
		t.Errorf("mocks_by_type[grpc]: want 1, got %d", byType["grpc"])
	}
}

// TestShutdownFlushLoopNoLeakUnderRace is the regression test for the
// post-Shutdown flush-goroutine leak. If ensureFlushLoop is called
// outside the recordingMu critical section, a hot-path thread can spawn
// the flush goroutine after Shutdown's phase 3 has already decided not
// to close closeCh — leaking a goroutine that waits forever on an
// unreachable ticker.
//
// The invariant we assert: after Shutdown returns, either the flush
// loop was never started, or flushDone is already closed (i.e. the
// goroutine exited cleanly). The test repeats across iterations to
// shake out scheduling orders.
func TestShutdownFlushLoopNoLeakUnderRace(t *testing.T) {
	const iterations = 20
	const workers = 100

	for i := 0; i < iterations; i++ {
		tel, _ := newTestTelemetry(t)

		var wg sync.WaitGroup
		for j := 0; j < workers; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tel.RecordedTestAndMocks()
				tel.RecordedTestCaseMock("http")
			}()
		}
		// Shutdown races with the hot-path goroutines: some finish
		// before Shutdown, some are mid-flight under recordingMu when
		// Shutdown tries to acquire it, some arrive after closed=true.
		tel.Shutdown()
		wg.Wait()

		if tel.flushLoopStarted.Load() {
			select {
			case <-tel.flushDone:
				// clean exit — loop terminated on closeCh
			default:
				t.Fatalf("iter %d: flush loop still running after Shutdown (goroutine leak)", i)
			}
		}
	}
}

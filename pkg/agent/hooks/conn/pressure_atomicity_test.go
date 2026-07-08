package conn

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// newCaptureResp builds a minimal, well-formed HTTP response that flows all the
// way through Capture (small body, no encoding, not filtered) so the only thing
// deciding whether a test case is emitted is the memory-pressure guard.
func newCaptureResp() *http.Response {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
}

// TestCapture_MockAndTCDroppedAtomicallyUnderPressure is the regression test for
// the go-memory-load-mongo (~75% CI failure) flake: under memory pressure a
// test case was persisted while one of its outgoing mocks had already been
// dropped by syncMock.AddMock, so replay reported "no mocks" / EOF for it.
//
// It drives the exact orphan scenario deterministically:
//
//   - pressure is active for a window; an outgoing mock whose request falls in
//     that window is offered to AddMock and DROPPED (the mock loss),
//   - pressure then CLEARS, so memoryguard.IsRecordingPaused() is false by the
//     time Capture runs (the case the old top-of-Capture guard missed),
//   - Capture is invoked with an exchange window that overlapped the pressure
//     interval.
//
// Invariant asserted: mock-drop and TC-drop are ATOMIC. Because a mock was
// dropped, the test case must NOT be emitted. Before the fix Capture only
// checked the live IsRecordingPaused() flag (false here) and emitted the TC —
// this test fails. After the fix Capture also checks whether pressure overlapped
// the window and drops the TC — this test passes.
func TestCapture_MockAndTCDroppedAtomicallyUnderPressure(t *testing.T) {
	logger := zap.NewNop()

	mgr := syncMock.New(logger)
	// Wire an output channel so AddMock takes its real forward/drop path
	// instead of the "unbound" buffer-and-warn branch.
	outCh := make(chan *models.Mock, 8)
	mgr.SetOutputChannel(outCh)

	// A wide window guarantees it contains the pressure interval we open below,
	// with no reliance on sleeps or wall-clock durations.
	reqTime := time.Now().Add(-time.Minute)

	// --- Simulate a pause mid-test-case and lose one of its mocks. ---
	mgr.SetMemoryPressure(true) // opens a pressure interval; memoryPause = true
	droppedMock := &models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: time.Now(), // inside the still-open pressure interval
			ResTimestampMock: time.Now(),
		},
	}
	mgr.AddMock(droppedMock)     // must be dropped: pressure active + req in interval
	mgr.SetMemoryPressure(false) // clears pressure -> IsRecordingPaused() is false now

	if _, dropped, _, _ := mgr.GetDropStats(); dropped != 1 {
		t.Fatalf("precondition: expected the outgoing mock to be dropped under pressure, got pressureDropped=%d", dropped)
	}
	// The dropped mock must NOT have been forwarded downstream.
	select {
	case m := <-outCh:
		t.Fatalf("precondition: dropped mock was unexpectedly forwarded: %+v", m)
	default:
	}

	resTime := time.Now().Add(time.Minute) // window [reqTime,resTime] overlaps the pressure interval

	req, err := http.NewRequest(http.MethodGet, "http://example.com/api/items", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp := newCaptureResp()

	ctx := syncMock.NewContext(context.Background(), mgr)
	tc := make(chan *models.TestCase, 1)

	// synchronous=false so Capture does not itself call ResolveRange; the only
	// decision under test is whether the test case is emitted.
	Capture(ctx, logger, tc, req, resp, reqTime, resTime, models.IncomingOptions{}, false, false, 8080)

	select {
	case got := <-tc:
		t.Fatalf("atomicity violated: a mock for this window was dropped, but Capture still emitted the test case %q — replay would report no_mocks for it", got.Name)
	default:
		// expected: the test case was dropped together with its mock.
	}
}

// pruneWait must exceed syncMock's pressureRangeStaleness (7s), the horizon
// after which SetMemoryPressure prunes a CLOSED pressure range. It is
// duplicated here (the const is unexported in package manager) with margin for
// scheduler jitter so the prune in TestCapture_PruneRace_CaptureCheckLoadBearing
// is deterministic, not flaky.
const pruneWait = 7500 * time.Millisecond

// TestCapture_PruneRace_CaptureCheckLoadBearing reproduces the ACTUAL prune race
// behind the go-memory-load-mongo flake and proves the Capture-level pressure
// check is load-bearing — not the softer "pressure cleared but range still
// present" case above, but the real one where the range is GONE by the time the
// late routes/record.go check runs.
//
// The record pipeline has two pressure checkpoints for one test case:
//
//	Stage 1  Capture() — the early check THIS fix adds (util.go). It runs in the
//	         capture goroutine, close to window-close, while the range is fresh.
//	Stage 2  routes/record.go:372 WasPressureActiveInWindow(req,resp) — the late
//	         check, run only when the TC is dequeued from the ~100-deep tcChan and
//	         streamed to the CLI. Under load this lags window-close by the tcChan
//	         buffer depth + build + handoff.
//
// syncMock prunes a closed pressure range 7s after it ends (SetMemoryPressure's
// staleness reaper). If Stage 2 lags past that horizon, the range that dropped
// the mock is already pruned, WasPressureActiveInWindow returns false, and the
// orphaned TC is streamed to disk -> replay hits no_mocks/EOF -> >4% tolerance ->
// the lane fails. That is the ~75%-red flake.
//
// This test drives the REAL Capture and the REAL record.go decision function
// (WasPressureActiveInWindow, the exact call at record.go:372), with a REAL
// prune performed by the REAL SetMemoryPressure reaper in between:
//
//	WITH the fix:    Stage 1 suppresses the TC (range still present) -> nothing
//	                 ever reaches Stage 2 -> no orphan.
//	WITHOUT the fix: Stage 1 emits the TC (old Capture only checked the live
//	                 IsRecordingPaused flag, false here) -> Stage 2 runs after the
//	                 prune -> range gone -> TC slips through -> orphan on disk.
//
// Revert the `|| pressureOverlappedWindow(...)` term in Capture and this test
// fails; restore it and it passes. That is the load-bearing proof.
//
// HONEST SCOPE: this proves the early check catches an orphan the late check
// misses once the range is pruned. It does NOT prove the early check is
// race-free — Stage 1 itself runs in a captureHookSem-gated goroutine whose
// respTimestamp is stamped before the semaphore acquire, so under sustained
// overload Stage 1 can also lag past the prune horizon. The fix is a race-WINDOW
// REDUCTION (it removes the tcChan buffer + build + handoff from the check lag),
// not a hard atomicity guarantee. See util.go's Capture comment.
func TestCapture_PruneRace_CaptureCheckLoadBearing(t *testing.T) {
	logger := zap.NewNop()

	mgr := syncMock.New(logger)
	outCh := make(chan *models.Mock, 8)
	mgr.SetOutputChannel(outCh)

	// --- Open a pressure interval and lose one in-window mock. ---
	mgr.SetMemoryPressure(true)
	reqTime := time.Now()
	droppedMock := &models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: time.Now(), // inside the still-open pressure interval
			ResTimestampMock: time.Now(),
		},
	}
	mgr.AddMock(droppedMock)     // dropped: pressure active + req in interval
	resTime := time.Now()        // window [reqTime,resTime] overlaps the interval
	mgr.SetMemoryPressure(false) // close the interval (end ~= now); range still PRESENT

	if _, dropped, _, _ := mgr.GetDropStats(); dropped != 1 {
		t.Fatalf("precondition: expected the outgoing mock to be dropped under pressure, got pressureDropped=%d", dropped)
	}
	// Sanity: while the range is fresh, BOTH checkpoints would catch the overlap.
	// The bug is purely about WHEN Stage 2 runs — so confirm it is catchable now.
	if overlap, _ := mgr.WasPressureActiveInWindow(reqTime, resTime); !overlap {
		t.Fatalf("precondition: expected pressure overlap to be visible before the prune")
	}

	// === Stage 1: the Capture-level early check (the fix). Ranges are fresh. ===
	ctx := syncMock.NewContext(context.Background(), mgr)
	tc := make(chan *models.TestCase, 1)
	req, err := http.NewRequest(http.MethodGet, "http://example.com/api/items", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	Capture(ctx, logger, tc, req, newCaptureResp(), reqTime, resTime, models.IncomingOptions{}, false, false, 8080)

	var emitted *models.TestCase
	select {
	case emitted = <-tc:
	default:
	}

	// === Prune: cross the staleness horizon, then fire the REAL reaper. ===
	// SetMemoryPressure prunes closed ranges older than pressureRangeStaleness on
	// every call; this false->false call is a pure prune trigger (no buffer wipe,
	// which only runs on the ->true transition).
	time.Sleep(pruneWait)
	mgr.SetMemoryPressure(false)

	// The range that dropped the mock is now gone: the late check can no longer
	// see the overlap.
	if overlap, _ := mgr.WasPressureActiveInWindow(reqTime, resTime); overlap {
		t.Fatalf("precondition: expected the closed pressure range to be pruned after %s", pruneWait)
	}

	// === Stage 2: the late routes/record.go check, faithfully invoked. ===
	// This is exactly record.go:372: WasPressureActiveInWindow over the TC's
	// [HTTPReq.Timestamp, HTTPResp.Timestamp]. If Stage 1 already dropped the TC
	// there is nothing to check; if it emitted one, the pruned range lets it slip.
	var survived *models.TestCase
	if emitted != nil {
		if lateOverlap, _ := mgr.WasPressureActiveInWindow(emitted.HTTPReq.Timestamp, emitted.HTTPResp.Timestamp); !lateOverlap {
			survived = emitted // record.go streams it to disk -> orphan
		}
	}

	if survived != nil {
		t.Fatalf("prune race: orphan TC %q reached disk — its mock was dropped under pressure, "+
			"but the late record.go check missed it because the range was pruned. The Capture-level "+
			"check must suppress it at window-close.", survived.Name)
	}
}

// TestCapture_TCKeptWhenNoPressureOverlap is the companion case proving the fix
// does not over-suppress: a calm window (no pressure interval overlaps it) whose
// mock is kept must still have its test case emitted.
func TestCapture_TCKeptWhenNoPressureOverlap(t *testing.T) {
	logger := zap.NewNop()

	mgr := syncMock.New(logger)
	outCh := make(chan *models.Mock, 8)
	mgr.SetOutputChannel(outCh)

	// No SetMemoryPressure at all -> no pressure ranges -> nothing overlaps.
	keptMock := &models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: time.Now(),
			ResTimestampMock: time.Now(),
		},
	}
	mgr.AddMock(keptMock) // no pressure -> kept, forwarded to outCh

	select {
	case <-outCh:
		// expected: mock kept and forwarded.
	default:
		t.Fatalf("precondition: expected the mock to be forwarded when no pressure is active")
	}

	reqTime := time.Now().Add(-time.Minute)
	resTime := time.Now().Add(time.Minute)

	req, err := http.NewRequest(http.MethodGet, "http://example.com/api/items", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp := newCaptureResp()

	ctx := syncMock.NewContext(context.Background(), mgr)
	tc := make(chan *models.TestCase, 1)

	Capture(ctx, logger, tc, req, resp, reqTime, resTime, models.IncomingOptions{}, false, false, 8080)

	select {
	case got := <-tc:
		if got.HTTPResp.StatusCode != http.StatusOK {
			t.Fatalf("expected a captured test case with status 200, got %d", got.HTTPResp.StatusCode)
		}
		// expected: calm test case kept.
	default:
		t.Fatalf("expected the test case to be emitted when no pressure overlapped its window")
	}
}

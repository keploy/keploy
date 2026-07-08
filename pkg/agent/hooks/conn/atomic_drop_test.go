package conn

import (
	"context"
	"net/http"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestCapture_TCDroppedByLedger is the load-bearing regression test for the
// atomic mock-drop / test-case-drop fix (#4336). It isolates the NEW mechanism
// from the older pressure-range overlap heuristic:
//
//   - No memory-pressure range is ever opened on the manager, so
//     PressureEverActive() is false and pressureOverlappedWindow() short-circuits
//     to false. The ONLY thing that can suppress the test case here is the
//     dropped-mock ledger term (droppedMockInWindow -> TCHasDroppedMock).
//
//   - A mock owned by the exchange window is recorded as dropped via
//     RecordDroppedMock — exactly what supervisor.Session.emitMockCore does when
//     the relay tee gates a chunk under memory pressure and the mongo mock is
//     abandoned. Its owning HTTP test case must therefore NOT be emitted, or
//     replay would report no_mocks / candidates:0 for it.
//
// Revert the `|| droppedMockInWindow(...)` term in Capture and this test fails
// (the TC is emitted with a dropped mock). Restore it and it passes. That is the
// load-bearing proof.
func TestCapture_TCDroppedByLedger(t *testing.T) {
	logger := zap.NewNop()

	mgr := syncMock.New(logger)
	outCh := make(chan *models.Mock, 8)
	mgr.SetOutputChannel(outCh)

	// The exchange window and a dropped mock whose request timestamp falls
	// inside it. No SetMemoryPressure — this proves the ledger, not pressure
	// ranges, is what suppresses the TC.
	reqTime := time.Now().Add(-time.Minute)
	droppedReqTs := time.Now() // inside [reqTime, resTime]
	resTime := time.Now().Add(time.Minute)

	mgr.RecordDroppedMock(droppedReqTs)

	if !mgr.TCHasDroppedMock(reqTime, resTime) {
		t.Fatalf("precondition: expected the recorded drop to be owned by the window")
	}
	if mgr.PressureEverActive() {
		t.Fatalf("precondition: no pressure range should exist in this test — the drop must be the only suppression signal")
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com/api/items", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	ctx := syncMock.NewContext(context.Background(), mgr)
	tc := make(chan *models.TestCase, 1)

	// synchronous=false so Capture does not itself call ResolveRange (which
	// resolves via the package global); the only decision under test is whether
	// the test case is emitted, and it must read the ctx-carried manager.
	Capture(ctx, logger, tc, req, newCaptureResp(), reqTime, resTime, models.IncomingOptions{}, false, false, 8080)

	select {
	case got := <-tc:
		t.Fatalf("atomicity violated: a mock owned by this window was dropped, but Capture still emitted the test case %q — replay would report no_mocks for it", got.Name)
	default:
		// expected: the test case was dropped together with its mock.
	}
}

// TestCapture_TCKeptWhenNoDrop is the companion proving the ledger gate does not
// over-suppress: a window that owns no dropped mock must still have its test
// case emitted.
func TestCapture_TCKeptWhenNoDrop(t *testing.T) {
	logger := zap.NewNop()

	mgr := syncMock.New(logger)
	outCh := make(chan *models.Mock, 8)
	mgr.SetOutputChannel(outCh)

	// Record a drop that is OUTSIDE this window (an unrelated earlier exchange)
	// to prove the gate is window-scoped, not a global kill-switch.
	mgr.RecordDroppedMock(time.Now().Add(-time.Hour))

	reqTime := time.Now().Add(-time.Minute)
	resTime := time.Now().Add(time.Minute)

	req, err := http.NewRequest(http.MethodGet, "http://example.com/api/items", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	ctx := syncMock.NewContext(context.Background(), mgr)
	tc := make(chan *models.TestCase, 1)

	Capture(ctx, logger, tc, req, newCaptureResp(), reqTime, resTime, models.IncomingOptions{}, false, false, 8080)

	select {
	case got := <-tc:
		if got.HTTPResp.StatusCode != http.StatusOK {
			t.Fatalf("expected a captured test case with status 200, got %d", got.HTTPResp.StatusCode)
		}
		// expected: TC kept — the only recorded drop is outside its window.
	default:
		t.Fatalf("over-suppression: no mock owned by this window was dropped, but Capture withheld the test case")
	}
}

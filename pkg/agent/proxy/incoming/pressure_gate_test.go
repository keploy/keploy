package proxy

import (
	"context"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

// TestPressureOverlappedExchange covers the window-close capture gate that is
// the authoritative, low-lag pressure check for the go-memory-load-mongo flake.
// It runs SYNCHRONOUSLY in the parser hot loop (before the captureHookSem
// acquire), so unlike the in-goroutine conn.Capture check it cannot inherit the
// tcChan backpressure lag that let the pressure range be pruned before the check
// ran. This test pins the decision table the gate relies on.
func TestPressureOverlappedExchange(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	ctx := syncMock.NewContext(context.Background(), mgr)

	reqTs := time.Now()
	respTs := reqTs.Add(10 * time.Millisecond)

	// 1. No pressure has ever activated: the lock-free fast path must return
	//    false without consulting any range (PressureEverActive is false).
	if mgr.PressureEverActive() {
		t.Fatalf("precondition: PressureEverActive should be false before any pressure")
	}
	if pressureOverlappedExchange(ctx, reqTs, respTs) {
		t.Fatalf("no pressure ever: expected overlap=false")
	}

	// 2. Pressure active, its (still-open) range overlaps the window → true.
	mgr.SetMemoryPressure(true)
	if !mgr.PressureEverActive() {
		t.Fatalf("PressureEverActive must latch true after activation")
	}
	if !pressureOverlappedExchange(ctx, reqTs, respTs) {
		t.Fatalf("pressure active over window: expected overlap=true")
	}

	// 3. Pressure CLEARED but the closed range is still present (not yet pruned)
	//    and still overlaps the window → true. This is the case the plain
	//    IsRecordingPaused check misses and the flake exploited.
	mgr.SetMemoryPressure(false)
	if !pressureOverlappedExchange(ctx, reqTs, respTs) {
		t.Fatalf("pressure cleared but range present & overlapping: expected overlap=true")
	}

	// 4. A window strictly AFTER the closed range does not overlap → false
	//    (no over-suppression of calm exchanges).
	calmReq := respTs.Add(time.Second)
	calmResp := calmReq.Add(10 * time.Millisecond)
	if pressureOverlappedExchange(ctx, calmReq, calmResp) {
		t.Fatalf("calm window after the range: expected overlap=false")
	}
}

// TestPressureEverActive_FastPathNil verifies the nil-manager and never-pressured
// fast paths so the hot loop stays lock-free on healthy runs.
func TestPressureEverActive_FastPathNil(t *testing.T) {
	var nilMgr *syncMock.SyncMockManager
	if nilMgr.PressureEverActive() {
		t.Fatalf("nil manager must report PressureEverActive=false")
	}
	mgr := syncMock.New(zap.NewNop())
	if mgr.PressureEverActive() {
		t.Fatalf("fresh manager must report PressureEverActive=false")
	}
}

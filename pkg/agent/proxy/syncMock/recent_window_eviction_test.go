package manager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestLateOwnedMockSurvivesWindowBurst is the regression for the
// go-memory-load-mongo replay `no_mocks` flake.
//
// Scenario (exactly what the summary endpoint does under load): a test case
// issues a small `find` (fast decode, mapped in-window) AND a large `aggregate`
// (a big Mongo response that finishes decoding much later, so it lands in the
// buffer AFTER its HTTP window resolved and must be retro-binned to that
// window). Meanwhile the recorder keeps resolving new test windows at a high
// burst rate.
//
// The bug: recentWindows was bounded ONLY by a 256-entry COUNT cap (its
// documented 7s age-prune was a no-op). Under a burst, >256 windows resolve
// within the ~7s the stale-cutoff keeps the aggregate reachable, so the
// aggregate's owning window is evicted from the ring BEFORE its slow decode
// lands. ownerWindow() then misses, the aggregate is never attributed to its
// test, and replay serves an empty per-test pool for the aggregate query
// (match_phase=no_mocks). The fast `find` — mapped in-window immediately —
// survives, so the test ends with exactly one of its two mocks.
//
// The fix keeps a window queryable for the full 7s staleness horizon (real
// age-prune + a count cap large enough to hold a realistic burst), so the late
// aggregate always finds its owning window and is retro-binned.
//
// This test drives the manager directly (no e2e): it resolves a window, then
// bursts far more than the OLD 256-cap of later windows — all within the 7s
// horizon — before the late owned mock arrives. Pre-fix (256 cap) the owning
// window is evicted and the mock is orphaned (no late mapping); post-fix it is
// retained and the mock is retro-binned to its test.
func TestLateOwnedMockSurvivesWindowBurst(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1<<16)
	mapCh := make(chan models.TestMockMapping, 1<<16)
	mgr := &SyncMockManager{}
	mgr.SetOutputChannel(ch)
	mgr.SetMappingChannel(context.Background(), mapCh)
	mgr.SetFirstRequestSignaled()

	// All timestamps are recent (base ~= now) so the 7s stale-cutoff / age-prune
	// keeps EVERY window and the late mock in play — the ONLY variable under test
	// is ring eviction by the count cap.
	base := time.Now()

	// The summary test's HTTP window W. Its small `find` maps in-window here;
	// the large `aggregate` is still decoding, so the buffer is empty for W.
	wStart := base
	wEnd := base.Add(5 * time.Millisecond)
	mgr.ResolveRange(wStart, wEnd, "summary-42", true, true)

	// Burst: resolve far more than the old 256-cap of later, non-overlapping
	// windows, all packed within a few hundred ms (i.e. well inside 7s — exactly
	// what go-memory-load-mongo does: ~1000 windows resolve within one 7s span).
	const burst = 512 // > old maxRecentWindows (256), << new cap (8192)
	for i := 0; i < burst; i++ {
		s := wEnd.Add(time.Duration(i+1) * time.Millisecond)
		mgr.ResolveRange(s, s, fmt.Sprintf("later-%d", i), true, true)
	}

	// Now the large aggregate finally finishes decoding and lands in the buffer.
	// Its recorded ReqTimestampMock lies INSIDE W (it was issued during the
	// summary request), so it belongs to "summary-42".
	aggTs := wStart.Add(2 * time.Millisecond)
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer,
		&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: aggTs}},
	)
	mgr.mu.Unlock()

	// One periodic owned-window flush (the 1s ticker path) retro-bins owned late
	// mocks. It can only attribute the aggregate if W is still in recentWindows.
	mgr.FlushOwnedWindows()

	// The aggregate must be attributed to summary-42: it has a valid owning
	// window, so it must NOT be left orphaned in the buffer.
	mgr.mu.Lock()
	remaining := len(mgr.buffer)
	mgr.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("late aggregate was orphaned: its owning window (summary-42) was evicted from recentWindows before the mock arrived, so ownerWindow() missed and the mock is stranded in the buffer (%d left). This is the go-memory-load-mongo no_mocks bug.", remaining)
	}

	// And it must have produced a late mapping for summary-42 (the delta that
	// completes the test's per-test mock pool at replay).
	var lateForSummary int
	for drained := false; !drained; {
		select {
		case mp := <-mapCh:
			if mp.TestName == "summary-42" {
				lateForSummary += len(mp.MockIDs)
			}
		default:
			drained = true
		}
	}
	if lateForSummary < 1 {
		t.Fatalf("expected a late TestMockMapping attributing the aggregate to summary-42; got none — the aggregate was not retro-binned to its owning window")
	}
}

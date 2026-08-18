// Package proxy — real per-call DRAINING semantics of MockManager.GetConsumedMocks.
//
// The reset-resend safety gate in pkg/service/replay depends on GetConsumedMocks
// being per-call and draining (it must report ONLY the mocks consumed since the
// last drain, then clear its list). Earlier replay-side tests stubbed
// GetConsumedMocks and so never exercised this contract; this test pins it on
// the REAL MockManager so a regression in the drain can't silently break the
// gate's "this request consumed >0 mocks" check.
package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestGetConsumedMocks_PerCallDrains records a consumption into a real
// MockManager and asserts:
//   - the first GetConsumedMocks reports exactly that consumption,
//   - a second GetConsumedMocks returns empty (the list was DRAINED),
//   - a fresh consumption after the drain is reported on its own (the count is
//     per-call, not cumulative).
//
// This is the exact contract the reset-resend gate relies on: a non-empty
// per-call result == "this request consumed a mock" == unsafe to re-send.
func TestGetConsumedMocks_PerCallDrains(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Before any consumption the list is empty.
	if got := mm.GetConsumedMocks(); len(got) != 0 {
		t.Fatalf("expected 0 consumed mocks initially, got %d", len(got))
	}

	// Record one consumption via the public seam (MarkMockAsUsed flags the mock
	// as used, the same record path DeleteFilteredMock/UpdateMock drive).
	mock := models.Mock{Name: "mongo-find-1", Kind: models.Mongo}
	if !mm.MarkMockAsUsed(mock) {
		t.Fatal("MarkMockAsUsed should record a consumption for a named mock")
	}

	// First drain reports exactly the one consumed mock.
	first := mm.GetConsumedMocks()
	if len(first) != 1 || first[0].Name != "mongo-find-1" {
		t.Fatalf("expected first drain to report [mongo-find-1], got %#v", first)
	}

	// Second drain MUST be empty — the previous call drained the list. If this
	// regresses, the gate's len(consumed) > 0 check would see stale data and
	// could refuse a safe re-send (or, with a cumulative baseline, wrongly allow
	// an unsafe one — the original bug).
	if second := mm.GetConsumedMocks(); len(second) != 0 {
		t.Fatalf("expected second drain to be empty (drained), got %d: %#v", len(second), second)
	}

	// A fresh consumption is reported on its own, proving the count is per-call
	// rather than cumulative across the manager's lifetime.
	if !mm.MarkMockAsUsed(models.Mock{Name: "mongo-find-2", Kind: models.Mongo}) {
		t.Fatal("MarkMockAsUsed should record the second consumption")
	}
	third := mm.GetConsumedMocks()
	if len(third) != 1 || third[0].Name != "mongo-find-2" {
		t.Fatalf("expected per-call drain to report only [mongo-find-2], got %#v", third)
	}
}

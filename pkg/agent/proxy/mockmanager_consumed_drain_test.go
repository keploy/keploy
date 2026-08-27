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

// TestTotalConsumedMocks_DoesNotDrain pins the complement of the contract
// above. The /agent/scope/begin re-narrow rebuilds the served pool from the
// pristine store, so it must be able to ask "which mocks have already been
// served?" — but it must NOT answer that question with GetConsumedMocks,
// whose drain is what the CLI's end-of-run `mock replay summary` counts.
//
// So: TotalConsumedMocks accumulates across the manager's lifetime and never
// empties, and reading it leaves the drain untouched for the summary.
func TestTotalConsumedMocks_DoesNotDrain(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	if got := mm.TotalConsumedMocks(); len(got) != 0 {
		t.Fatalf("expected an empty cumulative ledger initially, got %d", len(got))
	}

	if !mm.MarkMockAsUsed(models.Mock{Name: "http-1", Kind: models.HTTP}) {
		t.Fatal("MarkMockAsUsed should record a consumption for a named mock")
	}
	if !mm.MarkMockAsUsed(models.Mock{Name: "http-2", Kind: models.HTTP}) {
		t.Fatal("MarkMockAsUsed should record the second consumption")
	}

	// Reading the cumulative ledger repeatedly must not empty it...
	for i := 0; i < 3; i++ {
		total := mm.TotalConsumedMocks()
		if len(total) != 2 {
			t.Fatalf("read %d: expected 2 cumulative entries, got %d: %#v", i, len(total), total)
		}
		if _, ok := total["http-1"]; !ok {
			t.Fatalf("read %d: http-1 missing from the cumulative ledger", i)
		}
	}

	// ...and must not have stolen the drain the summary depends on.
	if drained := mm.GetConsumedMocks(); len(drained) != 2 {
		t.Fatalf("reading the cumulative ledger must leave the drain intact; got %d", len(drained))
	}

	// After the drain, the cumulative ledger still remembers everything — that
	// is the whole point: a re-stage after a drain must not resurrect.
	total := mm.TotalConsumedMocks()
	if len(total) != 2 {
		t.Fatalf("cumulative ledger must survive a drain; got %d: %#v", len(total), total)
	}

	// The returned map is a snapshot: mutating it must not corrupt the ledger.
	delete(total, "http-1")
	if len(mm.TotalConsumedMocks()) != 2 {
		t.Fatal("TotalConsumedMocks must return a copy, not the live map")
	}
}

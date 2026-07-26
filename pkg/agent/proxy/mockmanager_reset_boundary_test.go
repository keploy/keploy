// Package proxy — PR A / Phase 0 test-set-boundary free tests.
//
// ResetForReplaySession runs at every replay test-set boundary
// (Proxy.Mock → ResetForReplaySession) BEFORE the controller ships and
// this manager decodes the next set. Phase 0 extends it to free the
// previous set's mock pool (filtered / unfiltered / startup trees, their
// per-kind + stateless companions, and hitIdx) and to run the registered
// boundary-reset hooks, so the old set is not resident while the next
// set's whole pool is decoded — the transition double-residency that
// drives the auto-replay agent OOM.
//
// These tests assert the OBSERVABLE contract (pool emptied, revisions
// advanced, manager still usable afterwards, hooks fired). The actual
// RSS drop is measured in the load repro harness, not here — a byte-level
// memory assertion in a unit test is inherently flaky (GC timing).
package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestResetForReplaySession_FreesMockTrees verifies that a boundary reset
// empties every mock tier, advances the revisions, and leaves the manager
// able to load the next set (i.e. the reset frees rather than wedges).
func TestResetForReplaySession_FreesMockTrees(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Second)

	// Load a set spanning all three request-time tiers.
	mStartup := newMockForTest("startup", start.Add(-1*time.Second), models.LifetimeSession)
	mPerTest := newMockForTest("perTest", start.Add(5*time.Second), models.LifetimePerTest)
	mSession := newMockForTest("session", start.Add(1*time.Second), models.LifetimeSession)
	mm.SetMocksWithWindow(
		[]*models.Mock{mStartup, mPerTest},
		[]*models.Mock{mSession},
		start, end,
	)

	// Sanity: all tiers populated before the reset.
	if got, _ := mm.GetFilteredMocks(); len(got) == 0 {
		t.Fatalf("precondition: per-test tree empty before reset")
	}
	if got, _ := mm.GetUnFilteredMocks(); len(got) == 0 {
		t.Fatalf("precondition: session tree empty before reset")
	}
	if got, _ := mm.GetStartupMocks(); len(got) == 0 {
		t.Fatalf("precondition: startup tree empty before reset")
	}
	// Stateless lookup map populated for the HTTP kind too.
	if f, _ := mm.GetStatelessMocks(models.HTTP, "perTest"); len(f) == 0 {
		t.Fatalf("precondition: stateless per-test map empty before reset")
	}

	revBefore := mm.Revision()
	kindRevBefore := mm.RevisionByKind(models.HTTP)

	mm.ResetForReplaySession()

	// Every tier must now be empty.
	if got, _ := mm.GetFilteredMocks(); len(got) != 0 {
		t.Fatalf("per-test tree: want empty after reset, got %d", len(got))
	}
	if got, _ := mm.GetUnFilteredMocks(); len(got) != 0 {
		t.Fatalf("session tree: want empty after reset, got %d", len(got))
	}
	if got, _ := mm.GetStartupMocks(); len(got) != 0 {
		t.Fatalf("startup tree: want empty after reset, got %d", len(got))
	}
	if f, u := mm.GetStatelessMocks(models.HTTP, "perTest"); len(f) != 0 || len(u) != 0 {
		t.Fatalf("stateless maps: want empty after reset, got f=%d u=%d", len(f), len(u))
	}

	// Revisions must have advanced so revision-gated parsers rebuild off
	// the empty pool instead of pinning the old cohort.
	if mm.Revision() == revBefore {
		t.Fatalf("global revision: want advanced after reset, still %d", revBefore)
	}
	if mm.RevisionByKind(models.HTTP) == kindRevBefore {
		t.Fatalf("HTTP kind revision: want advanced after reset, still %d", kindRevBefore)
	}

	// The manager must still accept the NEXT set — the reset frees, it
	// does not wedge.
	mNext := newMockForTest("next", start.Add(5*time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{mNext}, nil, start, end)
	got, _ := mm.GetFilteredMocks()
	if !containsMockNamed(got, "next") {
		t.Fatalf("next set not loaded after reset; got %d mocks", len(got))
	}
	if containsMockNamed(got, "perTest") {
		t.Fatalf("previous set's per-test mock leaked into next set after reset")
	}
}

// TestResetForReplaySession_RunsBoundaryHooks verifies the boundary-reset
// hook registry is invoked by ResetForReplaySession — the seam mongo/v2
// uses to drop its encoded-section LRU at the set boundary.
func TestResetForReplaySession_RunsBoundaryHooks(t *testing.T) {
	var calls int
	// No unregister exists by design (registration is process-global);
	// the closure counter is local so leaking the hook past this test is
	// harmless.
	integrations.RegisterBoundaryResetHook(func() { calls++ })

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	before := calls
	mm.ResetForReplaySession()
	if calls != before+1 {
		t.Fatalf("boundary reset hook: want invoked exactly once, delta=%d", calls-before)
	}
}

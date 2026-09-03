// Package proxy — equivalence proof for the agent-owned consumption map.
//
// The AgentOwnsConsumed re-architecture (models.MockFilterParams.AgentOwnsConsumed)
// stops the client from re-sending its accumulated TotalConsumedMocks every
// testcase and has the agent apply filterOutDeleted from its OWN persistent map
// instead. That is only correct if the agent's persistent map is IDENTICAL to
// what the client would have accumulated from the per-call GetConsumedMocks
// drains. This test pins that equivalence on the real MockManager so the
// re-architecture can never silently diverge (which would re-serve or wrongly
// drop mocks under load — the exact regression the flag guards against).
package proxy

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestGetPersistentConsumed_EqualsClientAccumulation drives several "testcases"
// of consumption (including a mock whose state CHANGES to Deleted on a later
// testcase, and per-call drains in between, exactly as replay drives it) and
// asserts the agent's never-drained GetPersistentConsumed equals the map the
// client builds by merging successive GetConsumedMocks drains by name.
func TestGetPersistentConsumed_EqualsClientAccumulation(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// The client's accumulation: after each testcase it drains and merges by
	// name (latest state wins) — exactly pkg/service/replay/replay.go's
	// `totalConsumedMocks[m.Name] = m`.
	client := map[string]models.MockState{}
	accumulate := func() {
		for _, ms := range mm.GetConsumedMocks() {
			client[ms.Name] = ms
		}
	}

	// Testcase 1: consume A (updated) and B (deleted).
	if err := mm.flagMockAsUsed(models.MockState{Name: "A", Kind: models.MySQL, Usage: models.Updated, IsFiltered: true, SortOrder: 1}); err != nil {
		t.Fatalf("flagMockAsUsed A: %v", err)
	}
	if err := mm.flagMockAsUsed(models.MockState{Name: "B", Kind: models.MySQL, Usage: models.Deleted, IsFiltered: false, SortOrder: 2}); err != nil {
		t.Fatalf("flagMockAsUsed B: %v", err)
	}
	accumulate()

	// Testcase 2: A's state CHANGES to deleted; consume C (updated). The latest
	// state per name is what filterOutDeleted reads, so this transition is the
	// load-bearing case.
	if err := mm.flagMockAsUsed(models.MockState{Name: "A", Kind: models.MySQL, Usage: models.Deleted, IsFiltered: true, SortOrder: 3}); err != nil {
		t.Fatalf("flagMockAsUsed A2: %v", err)
	}
	if err := mm.flagMockAsUsed(models.MockState{Name: "C", Kind: models.MySQL, Usage: models.Updated, IsFiltered: true, SortOrder: 4}); err != nil {
		t.Fatalf("flagMockAsUsed C: %v", err)
	}
	accumulate()

	persistent := mm.GetPersistentConsumed()

	// Core equivalence: the two maps MUST be identical (same keys, same latest
	// MockState per name). If they ever diverge, AgentOwnsConsumed is unsafe.
	if !reflect.DeepEqual(persistent, client) {
		t.Fatalf("agent persistent map != client accumulation\n persistent=%#v\n client=%#v", persistent, client)
	}

	// Spot-check the exact fields filterOutDeleted keys on (Usage, IsFiltered,
	// SortOrder) reflect the LATEST state, not the first.
	if got := persistent["A"]; got.Usage != models.Deleted || got.SortOrder != 3 {
		t.Fatalf("A must reflect its latest (deleted, sortOrder=3) state, got %+v", got)
	}
	if got := persistent["C"]; got.Usage != models.Updated {
		t.Fatalf("C must be updated, got %+v", got)
	}
	if _, ok := persistent["B"]; !ok {
		t.Fatal("B (deleted on testcase 1) must remain in the persistent map for filterOutDeleted")
	}
}

// TestGetPersistentConsumed_NotDrained proves the persistent map is NOT drained
// by GetConsumedMocks (the whole point — the drain is why the client had to
// re-feed state; the persistent map must survive it).
func TestGetPersistentConsumed_NotDrained(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	if err := mm.flagMockAsUsed(models.MockState{Name: "X", Kind: models.MySQL, Usage: models.Deleted}); err != nil {
		t.Fatalf("flagMockAsUsed: %v", err)
	}
	_ = mm.GetConsumedMocks() // drains consumedList
	_ = mm.GetConsumedMocks() // still drained

	if p := mm.GetPersistentConsumed(); len(p) != 1 || p["X"].Usage != models.Deleted {
		t.Fatalf("persistent map must survive GetConsumedMocks drains; got %#v", p)
	}
}

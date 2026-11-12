package models

import (
	"reflect"
	"testing"
)

// WithoutWorkerRuleEntries drops the worker-rule entries of a
// worker scope, keeps every other entry in its order, and hands back the
// scope itself when it holds none.
func TestWithoutWorkerRuleEntriesDropsOnlyTheMarkedEntries(t *testing.T) {
	scope := []MockEntry{
		{Name: "poll-1"},
		{Name: "jwks-1", Reason: WorkerRuleReason("jwks")},
		{Name: "poll-2", Reason: "moved by record's judging"},
		{Name: "jwks-2", Reason: WorkerRuleReason("jwks")},
	}
	got := WithoutWorkerRuleEntries(scope)
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if !reflect.DeepEqual(names, []string{"poll-1", "poll-2"}) {
		t.Fatalf("kept %v; want [poll-1 poll-2]", names)
	}
	if len(scope) != 4 || scope[1].Name != "jwks-1" {
		t.Fatalf("the scope passed in was changed: %+v", scope)
	}
	plain := scope[:1]
	if out := WithoutWorkerRuleEntries(plain); &out[0] != &plain[0] {
		t.Fatal("a scope with no worker-rule entries must be handed back as it is")
	}
	if WithoutWorkerRuleEntries(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

// ServeAsSession moves the named mocks out of the per-test pool into the
// session pool as copies stated and derived session lifetime, so every test
// can be answered by them and a loader that derives lifetimes again keeps
// them so. The stored mocks are not written.
func TestServeAsSessionMovesTheWorkerMocksToTheSessionPool(t *testing.T) {
	jwks := &Mock{Name: "mock-0", TestModeInfo: TestModeInfo{Lifetime: LifetimePerTest, IsFiltered: true}}
	own := &Mock{Name: "mock-1", TestModeInfo: TestModeInfo{Lifetime: LifetimePerTest, IsFiltered: true}}
	cfg := &Mock{Name: "mock-2", TestModeInfo: TestModeInfo{Lifetime: LifetimeSession}}

	filtered, unfiltered := ServeAsSession([]*Mock{jwks, own}, []*Mock{cfg}, []string{"mock-0", "absent"})

	if len(filtered) != 1 || filtered[0] != own {
		t.Fatalf("per-test pool = %v, want only mock-1", filtered)
	}
	if len(unfiltered) != 2 || unfiltered[0] != cfg || unfiltered[1].Name != "mock-0" {
		t.Fatalf("session pool = %v, want mock-2 then mock-0", unfiltered)
	}
	moved := unfiltered[1]
	if moved == jwks {
		t.Fatal("the stored mock must not be handed on: a copy is")
	}
	if ti := moved.TestModeInfo; ti.Lifetime != LifetimeSession || !ti.LifetimeDerived || ti.IsFiltered {
		t.Fatalf("moved mock = %+v, want a derived session mock", ti)
	}
	if jwks.TestModeInfo.Lifetime != LifetimePerTest || !jwks.TestModeInfo.IsFiltered {
		t.Fatalf("the stored mock was written: %+v", jwks.TestModeInfo)
	}
	if f2, u2 := ServeAsSession([]*Mock{own}, []*Mock{cfg}, nil); len(f2) != 1 || len(u2) != 1 {
		t.Fatalf("no names: got %v / %v", f2, u2)
	}
}

// WorkerRuleMockNames is the marked worker entries' names, each once, in order.
func TestWorkerRuleMockNamesReadsOnlyTheMarkedEntries(t *testing.T) {
	m := &Mapping{Worker: []MockEntry{
		{Name: "jwks-1", Reason: WorkerRuleReason("jwks")},
		{Name: "poll-1"},
		{Name: "", Reason: WorkerRuleReason("jwks")},
		{Name: "jwks-1", Reason: WorkerRuleReason("jwks")},
		{Name: "jwks-2", Reason: WorkerRuleReason("jwks")},
	}}
	if got := m.WorkerRuleMockNames(); !reflect.DeepEqual(got, []string{"jwks-1", "jwks-2"}) {
		t.Fatalf("got %v", got)
	}
	if (*Mapping)(nil).WorkerRuleMockNames() != nil {
		t.Fatal("a nil mapping has no worker-rule mocks")
	}
}

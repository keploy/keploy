package record

import (
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A testcase's mapping must include its ordinary (sync) egress mocks but MUST
// EXCLUDE async-egress mocks — even when the async mock's tempID is present in
// the mapping because its timestamp overlapped the testcase's request window.
// Async mocks are served at replay by the async engine from the full corpus, so
// per-test mapping them would wrongly bind a background delivery to a testcase.
func TestResolveMappingEntriesExcludesAsyncMocks(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}

	var correlationMap, asyncMockIDs sync.Map
	correlationMap.Store("sync-1", models.MockEntry{Name: "sync-1", Kind: "Http"})
	correlationMap.Store("async-1", models.MockEntry{Name: "async-1", Kind: "Http"})
	correlationMap.Store("async-2", models.MockEntry{Name: "async-2", Kind: "Http"})
	// async-1 and async-2 were stamped async by the AsyncRecorder hook.
	asyncMockIDs.Store("async-1", struct{}{})
	asyncMockIDs.Store("async-2", struct{}{})

	// The agent binned all three into this test's window (overlap), so all three
	// tempIDs appear in the mapping.
	mapping := models.TestMockMapping{
		TestName: "get-step-1",
		MockIDs:  []string{"sync-1", "async-1", "async-2"},
	}

	got := r.resolveMappingEntries(mapping, &correlationMap, &asyncMockIDs)

	if len(got) != 1 {
		t.Fatalf("want exactly the 1 sync mock in the mapping, got %d: %+v", len(got), got)
	}
	if got[0].Name != "sync-1" {
		t.Fatalf("want sync-1 mapped, got %q", got[0].Name)
	}
	// All resolved tempIDs (async included) are consumed from the correlation map.
	for _, id := range []string{"sync-1", "async-1", "async-2"} {
		if _, ok := correlationMap.Load(id); ok {
			t.Fatalf("tempID %q should have been consumed from correlationMap", id)
		}
	}
}

// With no async mocks, every correlated mock is mapped (baseline: the exclusion
// doesn't drop ordinary mocks).
func TestResolveMappingEntriesKeepsAllSyncMocks(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}
	var correlationMap, asyncMockIDs sync.Map
	correlationMap.Store("m1", models.MockEntry{Name: "m1"})
	correlationMap.Store("m2", models.MockEntry{Name: "m2"})

	got := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"m1", "m2"}},
		&correlationMap, &asyncMockIDs,
	)
	if len(got) != 2 {
		t.Fatalf("want both sync mocks mapped, got %d: %+v", len(got), got)
	}
}

package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// A connection that scopedFor wraps (a per-start table installed, or a worker
// scope registered, with the proven switch off) tells which gap between the
// recorded tests a time lies in as the manager does: the MySQL matcher reads
// it through integrations.RecordedWindowIndex to answer a call made between
// tests with the recording of its own gap. A view that lost it would leave
// the gaps unknown on such connections, and every call of a request
// recorded in several gaps would take the first gap's recording again.
func TestAScopedViewReportsTheManagersRecordedWindowIndex(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	idx, ok := view.(integrations.RecordedWindowIndex)
	if !ok {
		t.Fatal("a scoped view does not offer integrations.RecordedWindowIndex")
	}
	if i, known := idx.RecordedWindowIndex(pms(1500)); known || i != -1 {
		t.Fatalf("with no recorded windows seeded, the view reports index %d, known %v; want -1, false", i, known)
	}

	// windowsOf: test-1 records over [1000, 1100] ms, test-2 over [2000, 2100] ms.
	mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
	for _, c := range []struct {
		name string
		at   time.Time
		want int
	}{
		{"start-up", pms(500), -1},
		{"test-1's start", pms(1000), 0},
		{"inside test-1", pms(1050), 0},
		{"the gap after test-1", pms(1500), 0},
		{"inside test-2", pms(2050), 1},
		{"after the last test", pms(3000), 1},
	} {
		wantIdx, wantKnown := mm.RecordedWindowIndex(c.at)
		if wantIdx != c.want || !wantKnown {
			t.Fatalf("%s: the manager reports index %d, known %v; want %d, true", c.name, wantIdx, wantKnown, c.want)
		}
		if i, known := idx.RecordedWindowIndex(c.at); i != wantIdx || known != wantKnown {
			t.Errorf("%s: the scoped view reports index %d, known %v; the manager %d, %v", c.name, i, known, wantIdx, wantKnown)
		}
	}
}

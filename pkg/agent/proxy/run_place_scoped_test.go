package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// A store wrapped for a test's scope waits on the run's place as its manager
// does: the wait is marked idle, and the mark ends when the place changes.
func TestRunPlace_AScopedStoreWaitsAsItsManager(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows(placeWindows())
	var db integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	p, ok := db.(integrations.RunPlacer)
	if !ok {
		t.Fatal("a scoped store has no run place")
	}
	marked := 0
	_, waits := p.IdleUntilRunPlaceChanges(p.RunPlaceChanged(), func() func() {
		marked++
		return func() { marked-- }
	})
	if !waits || marked != 1 {
		t.Fatalf("waits %v, marked %d; want the wait marked idle", waits, marked)
	}
	mm.EndRunSet()
	if marked != 0 {
		t.Fatalf("marked %d after the place changed; want the mark ended", marked)
	}
}

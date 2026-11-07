package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A store wrapped for a test's scope waits on the run's place as its manager
// does: the wait is marked idle, and the mark ends when the place changes.
func TestRunPlace_AScopedStoreWaitsAsItsManager(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(placeWindows())
	var db integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	p, ok := db.(integrations.RunPlacer)
	if !ok {
		t.Fatal("a scoped store has no run place")
	}
	if got := p.RunPlace(); got.SetEnd || got.Windows.Len() != 3 {
		t.Fatalf("a scoped store's place %+v; want its manager's, before the set's end", got)
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
	if !p.RunPlace().SetEnd {
		t.Fatal("a scoped store's place is not at the set's end after its manager's")
	}
}

// A store wrapped for a test's scope hands out its manager's test
// recordings and claims a hold through it; one over a store without the
// facet hands out none and claims none.
func TestRunPlace_AScopedStoreClaimsAsItsManager(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	a := r3Mock("mock-a", placeAt(2), "GET", "/a", "", "a")
	a.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}
	mm.SetMocksWithWindow([]*models.Mock{a}, nil, placeAt(0), placeAt(10))
	p := integrations.MockMemDb(&scopedMockDb{MockMemDb: mm}).(integrations.RunPlacer)
	if got := p.OpenTestRecordings(); len(got) != 1 || got[0].Name != "mock-a" {
		t.Fatalf("a scoped store's test recordings %v; want its manager's", got)
	}
	if !p.ClaimHold("mock-a") || mm.ClaimHold("mock-a") {
		t.Fatal("a scoped store's claim is not its manager's")
	}
	bare := &scopedMockDb{MockMemDb: noPlaceDb{mm}}
	if got := bare.OpenTestRecordings(); got != nil {
		t.Fatalf("over a store without the facet: %v, want none", got)
	}
	if bare.ClaimHold("mock-a") {
		t.Fatal("over a store without the facet a hold is claimed")
	}
}

// noPlaceDb is a store without the run's place facet.
type noPlaceDb struct{ integrations.MockMemDb }

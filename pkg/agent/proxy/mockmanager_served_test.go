package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Every answer a mock gives is counted, a mock that answers again included
// (its first-use state does not change), and a new replay session starts
// the count over.
func TestServedCountsCountEveryAnswer(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	for i := 0; i < 2; i++ {
		if err := mm.flagMockAsUsed(models.MockState{Name: "mock-1", Usage: models.Updated}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mm.flagMockAsUsed(models.MockState{Name: "mock-2", Usage: models.Deleted}); err != nil {
		t.Fatal(err)
	}
	got := mm.ServedCounts()
	if got["mock-1"] != 2 || got["mock-2"] != 1 {
		t.Fatalf("counts %v", got)
	}
	got["mock-1"] = 99
	if mm.ServedCounts()["mock-1"] != 2 {
		t.Fatal("ServedCounts handed out its own map")
	}
	mm.ResetForReplaySession()
	if n := len(mm.ServedCounts()); n != 0 {
		t.Fatalf("a new session starts with %d counts", n)
	}
}

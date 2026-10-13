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

// The served observer (the learn step's earlier places of any kind) is told
// of a mock a parser was served, and only of one: a call that served nothing
// tells it nothing, and an observer taken away is told nothing more.
func TestServedObserverIsToldOfEveryServedMock(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	var told []string
	mm.SetServedObserver(func(m *models.Mock) { told = append(told, m.Name) })

	if !mm.MarkMockAsUsed(models.Mock{Name: "pg-1", Kind: models.Postgres}) {
		t.Fatal("a named mock was not marked as used")
	}
	if mm.MarkMockAsUsed(models.Mock{}) {
		t.Fatal("a mock with no name was marked as used")
	}
	if mm.DeleteFilteredMock(models.Mock{Name: "absent", Kind: models.HTTP}) {
		t.Fatal("a mock the manager does not hold was consumed")
	}
	if len(told) != 1 || told[0] != "pg-1" {
		t.Fatalf("observer told %v, want [pg-1]", told)
	}

	mm.SetServedObserver(nil)
	mm.MarkMockAsUsed(models.Mock{Name: "redis-1", Kind: models.REDIS})
	if len(told) != 1 {
		t.Fatalf("an observer taken away was told %v", told[1:])
	}
}

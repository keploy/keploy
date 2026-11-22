package proxy

import (
	"reflect"
	"testing"
	"time"

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
		if err := mm.flagMockAsUsed(models.MockState{Name: "mock-1", Usage: models.Updated}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := mm.flagMockAsUsed(models.MockState{Name: "mock-2", Usage: models.Deleted}, 0); err != nil {
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

// The served observer (the learn step's earlier places) is told of each mock
// a door consumed, once the consume went through, never of one a door refused
// (a claim that fails: nothing served it) or of a call that names no mock, and
// an observer taken away is told nothing more.
func TestServedObserverIsToldOfEveryServedMock(t *testing.T) {
	perTest := newMockForTest("taken", exactT0, models.LifetimePerTest)
	startup := newMockForTest("startup-1", exactT0.Add(time.Second), models.LifetimeSession)
	session := newMockForTest("session-1", exactT0.Add(2*time.Second), models.LifetimeSession)
	reused := newMockForTest("reused-1", exactT0.Add(3*time.Second), models.LifetimeSession)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow([]*models.Mock{perTest, startup}, []*models.Mock{session, reused}, models.BaseTime, time.Now())
	var told []string
	mm.SetServedObserver(func(m *models.Mock) { told = append(told, m.Name) })

	if !mm.MarkMockAsUsed(models.Mock{Name: "pg-1", Kind: models.Postgres}) {
		t.Fatal("a named mock was not marked as used")
	}
	if mm.MarkMockAsUsed(models.Mock{}) {
		t.Fatal("a mock with no name was marked as used")
	}
	if !mm.DeleteFilteredMock(*perTest) {
		t.Fatal("the per-test tier's mock was not consumed")
	}
	if mm.DeleteFilteredMock(*perTest) {
		t.Fatal("a mock another connection took was consumed again")
	}
	if !mm.DeleteUnFilteredMock(*session) {
		t.Fatal("the session tier's mock was not consumed")
	}
	if !mm.DeleteStartupMock(*startup) {
		t.Fatal("the startup tier's mock was not consumed")
	}
	if !mm.UpdateUnFilteredMock(reused, reused.DeepCopy()) {
		t.Fatal("the session tier's mock was not updated")
	}
	if want := []string{"pg-1", "taken", "session-1", "startup-1", "reused-1"}; !reflect.DeepEqual(told, want) {
		t.Fatalf("observer told %v, want %v", told, want)
	}

	mm.SetServedObserver(nil)
	mm.MarkMockAsUsed(models.Mock{Name: "redis-1", Kind: models.REDIS})
	if len(told) != 5 {
		t.Fatalf("an observer taken away was told %v", told[5:])
	}
}

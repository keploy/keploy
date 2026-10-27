package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// What a parser asks the manager to run once it lets its recordings go
// (integrations.Restaged) runs once, at the next staging, and at Close; at
// once when asked after Close; a later ask for a slot replaces the earlier;
// and through a worker's view, as through the manager.
func TestWhatAParserAskedToRunRunsOnceWhenTheManagerLetsItsRecordingsGo(t *testing.T) {
	now := time.Now()
	stage := func(mm *MockManager) {
		mm.SetMocksWithWindow(nil, []*models.Mock{r3Mock("mock-0", now, "GET", "/a", "", "{}")}, models.BaseTime, now.Add(time.Hour))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	ran := map[string]int{}
	ask := func(db integrations.MockMemDb, slot, name string) {
		if !integrations.WhenRestaged(db, slot, func() { ran[name]++ }) {
			t.Fatalf("%T does not tell when it lets its recordings go", db)
		}
	}
	ask(mm, "a", "first")
	ask(mm, "a", "replaced")
	ask(mm, "b", "b")
	if len(ran) != 0 {
		t.Fatalf("ran before any staging: %v", ran)
	}
	stage(mm)
	if ran["first"] != 0 || ran["replaced"] != 1 || ran["b"] != 1 {
		t.Fatalf("after a staging ran %v, want replaced and b once", ran)
	}
	stage(mm)
	if ran["replaced"] != 1 || ran["b"] != 1 {
		t.Fatalf("a staging ran again what an earlier one ran: %v", ran)
	}
	ask(&scopedMockDb{MockMemDb: mm}, "a", "view")
	stage(mm)
	if ran["view"] != 1 {
		t.Fatalf("asked through a worker's view, a staging ran it %d times", ran["view"])
	}
	ask(mm, "a", "closing")
	mm.Close()
	if ran["closing"] != 1 {
		t.Fatalf("Close ran it %d times", ran["closing"])
	}
	ask(mm, "a", "closed")
	if ran["closed"] != 1 {
		t.Fatalf("asked after Close, it ran %d times", ran["closed"])
	}
}

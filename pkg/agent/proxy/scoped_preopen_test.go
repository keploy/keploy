package proxy

import (
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
)

// In a run with the suite mark or an app-start mark, a connection the app opened
// outside a test (its pool at boot, before the first begin, or one opened
// between two tests) is served only the open test's mocks: its view is
// resolved from the app starts on every read, whenever it opened. (Without
// the marks a pid begin is refused: service/agent pidCannotScope.)
func TestAConnectionOpenedOutsideATestIsServedOnlyTheOpenTestsMocksWithTheMarks(t *testing.T) {
	mgr := livePool(t)
	pid := uint32(os.Getpid())
	p := &Proxy{}
	boot := p.scopedFor(pid, mgr)
	if boot == integrations.MockMemDb(mgr) {
		t.Fatal("a connection opened at boot kept the bare manager")
	}
	starts.Default.Begin(pid, "t1", "", false, time.Now())
	if got, want := readCounter(t, boot, 2), "a1,a2"; got != want {
		t.Fatalf("during t1 the boot connection served %s, want %s", got, want)
	}
	starts.Default.End(pid, "t1", false, time.Now())
	between := p.scopedFor(pid, mgr)
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	if got, want := readCounter(t, between, 3), "b1,b2,b2"; got != want {
		t.Fatalf("during t2 the connection opened between the tests served %s, want %s", got, want)
	}
	if got, want := readCounter(t, boot, 1), "b2"; got != want {
		t.Fatalf("during t2 the boot connection served %s, want %s", got, want)
	}
}

// TestScopedViewUnsetOwnBehaviorIdenticalWithBackgroundArmed proves B1: in
// KEPLOY_PROVEN unset mode with ServeExactBackground armed and an open test
// (so S12Background(mgr) is true), scopedMockDb.passThrough() MUST stay false
// so own calls on a scoped view (with the starts mark active or a registered
// worker) keep main's byStart narrowing, scope-keyed cursors, retained/held
// consumes, and MockSpent==false, while @startup background mocks still settle
// cleanly once through the background engine.
func TestScopedViewUnsetOwnBehaviorIdenticalWithBackgroundArmed(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	mgr := livePool(t)
	base := time.Now().Add(-2 * time.Hour)
	bgRecEntry := bgRec("mock-startup-bg", models.Mongo, 0)
	allMocks := append(counterPool(base.Add(time.Minute), "a1", "a2"), counterPool(base.Add(2*time.Minute), "b1", "b2")...)
	mgr.SetScopeMocks(nil, allMocks, false)
	mgr.SetMocksWithWindow(nil, allMocks, models.BaseTime, time.Now())
	mgr.SeedBackgroundRecordings([]models.BackgroundRecording{bgRecEntry})
	mgr.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-startup-bg"}, nil)

	pid := uint32(os.Getpid())
	p := &Proxy{}
	boot := p.scopedFor(pid, mgr)
	scoped, ok := boot.(*scopedMockDb)
	if !ok {
		t.Fatalf("expected *scopedMockDb with starts mark active, got %T", boot)
	}

	// Open t1 both in starts.Default (live narrowing) and MockManager scope
	// (so S12Background(mgr) is active in unset mode).
	starts.Default.Begin(pid, "t1", "", false, time.Now())
	mgr.SetScopeMocks(nil, allMocks, true)
	if !integrations.S12Background(boot) {
		t.Fatal("expected S12Background to be active during open test with ServeExactBackground armed")
	}
	if scoped.passThrough() {
		t.Fatal("scopedMockDb.passThrough() must stay false in unset mode even when S12Background is active")
	}
	if scoped.MockSpent("mock-startup-bg") {
		t.Fatal("scopedMockDb.MockSpent must stay false when not in exact serving")
	}

	// 1) Own calls keep byStart narrowing and scope-keyed cursors identically to main.
	if got, want := readCounter(t, boot, 2), "a1,a2"; got != want {
		t.Fatalf("during t1 with S12Background armed, boot served %s, want %s", got, want)
	}

	// 2) Background @startup mock still reads and consumes once cleanly through the scoped view.
	bg := scoped.Background(models.Mongo)
	if len(bg) != 1 || bg[0].Name != "mock-startup-bg" {
		t.Fatalf("expected 1 @startup background mock via scoped view, got %d", len(bg))
	}
	if !boot.DeleteStartupMock(*bg[0]) {
		t.Fatal("expected @startup background mock to consume once via scoped view")
	}
	if boot.DeleteStartupMock(*bg[0]) {
		t.Fatal("expected spent @startup background mock to refuse second consume")
	}

	// 3) Switching to t2 resets the scoped cursor and narrows to t2's mocks only.
	starts.Default.End(pid, "t1", false, time.Now())
	mgr.SetScopeMocks(nil, allMocks, false)
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	mgr.SetScopeMocks(nil, allMocks, true)
	if got, want := readCounter(t, boot, 3), "b1,b2,b2"; got != want {
		t.Fatalf("during t2 with S12Background armed, boot served %s, want %s", got, want)
	}

	// 4) Registered worker view with holdUsedUp retains own consumes in scope
	// instead of deleting them from the underlying manager.
	starts.Default.Reset()
	own1 := &models.Mock{Name: "w1-own", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: base.Add(3 * time.Minute)}}
	own2 := &models.Mock{Name: "w2-own", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: base.Add(4 * time.Minute)}}
	wmgr := newCursorManager(t)
	wmgr.SetMocksWithWindow([]*models.Mock{own1, own2}, nil, models.BaseTime, time.Now())
	wmgr.SeedBackgroundRecordings([]models.BackgroundRecording{bgRecEntry})
	wmgr.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-startup-bg"}, nil)
	wmgr.SetScopeMocks([]*models.Mock{own1, own2}, nil, true)
	wp := &Proxy{}
	wp.SetMappedUniverse([]string{"w1-own", "w2-own"})
	wp.SetWorkerScope(pid, []string{"w1-own"})
	wview := wp.scopedFor(pid, wmgr)
	gotFiltered, err := wview.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow failed: %v", err)
	}
	if len(gotFiltered) != 1 || gotFiltered[0].Name != "w1-own" {
		t.Fatalf("worker view must narrow to w1-own even with S12Background armed; got %v", scopedNames(gotFiltered))
	}
	if !wview.DeleteFilteredMock(*own1) {
		t.Fatal("expected DeleteFilteredMock(w1-own) to succeed via holdUsedUp")
	}
	// Because passThrough is false, own1 was held in usedUp for this scope, NOT
	// deleted from wmgr's filtered tree.
	underlying, err := wmgr.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("underlying GetPerTestMocksInWindow failed: %v", err)
	}
	found := false
	for _, mk := range underlying {
		if mk.Name == "w1-own" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("scoped worker DeleteFilteredMock deleted w1-own from underlying store instead of holding it in scope")
	}
}

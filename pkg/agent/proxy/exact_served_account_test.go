package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// heldBy reports whether list holds a mock of m's name and kind.
func heldBy(list []*models.Mock, m *models.Mock) bool {
	for _, l := range list {
		if l != nil && l.Name == m.Name && l.Kind == m.Kind {
			return true
		}
	}
	return false
}

// The open test's view accounts every mock a parser is served while it is
// installed (ServedInOpenTest), of any lifetime and through any door: the
// test's own (a delete door), a protocol-phase one that stays reusable (the
// update door, served twice, filed once), a protocol-phase one spent through
// a delete door, a worker-scope one, and a serve whose claim is refused
// (another test's recording, which the view does not show). The
// test's own answered list (ConsumedTestMocks) holds the test's own alone. A
// pool view files nothing, and the next test's view starts empty. Every mock
// the store's account holds is one the learn tracker heard of in its running
// test; when the tracker begins the next test over the last one's view (no
// pool view between them), the store still holds them and the tracker files
// them before its running test: the disagreement a learn step refuses on.
func TestTheOpenTestsAccountHoldsEveryServeOfAnyLifetime(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	hs := configMock("handshake", time.Second)
	setCfg := configMock("set-config", 2*time.Second)
	worker := httpCall("w-boot", 3*time.Second, "/receive")
	pool := httpCall("pool", 4*time.Second, "/pool")
	other := exactMock("other", 5*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own, other}, []*models.Mock{hs, setCfg, worker, pool})
	tr := learn.NewTracker()
	mm.SetServedObserver(tr.ServedMock)

	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactPool("k", []string{"pool"})
	if got := pollFirst(t, mm, "pool"); got != "pool" {
		t.Fatalf("the pool did not answer between tests: %s", got)
	}
	equalNames(t, "no test open", mm.ServedInOpenTest())

	tr.Begin()
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	equalNames(t, "a test just begun: the pool's serve is not its", mm.ServedInOpenTest())

	if !mm.DeleteFilteredMock(*own) {
		t.Fatal("the open test's own mock was refused")
	}
	mm.UpdateUnFilteredMock(hs, hs)
	mm.UpdateUnFilteredMock(hs, hs)
	if !mm.DeleteUnFilteredMock(*setCfg) {
		t.Fatal("a protocol-phase mock the view shows was refused at a delete door")
	}
	if got := pollFirst(t, mm, "w-boot"); got != "w-boot" {
		t.Fatalf("the reached worker mock did not answer in the test: %s", got)
	}
	if mm.DeleteFilteredMock(*own) {
		t.Fatal("the open test's own mock answered twice")
	}
	if mm.MarkMockAsUsed(*other) {
		t.Fatal("another test's recording answered in the open test")
	}
	equalNames(t, "the open test's account", mm.ServedInOpenTest(), "own", "handshake", "set-config", "w-boot", "other")
	equalNames(t, "the open test's own answered", mm.ConsumedTestMocks(), "own")
	_, _, served, _ := tr.Test()
	for _, m := range mm.ServedInOpenTest() {
		if !heldBy(served, m) {
			t.Fatalf("the store's account holds %s, which the tracker did not hear of in its running test", m.Name)
		}
	}

	// The next test begun over T1's view: the store's account is T1's, and
	// the tracker files every mock of it before its running test.
	tr.End()
	tr.Begin()
	_, _, served, before := tr.Test()
	acct := mm.ServedInOpenTest()
	equalNames(t, "begun over the last test's view", acct, "own", "handshake", "set-config", "w-boot", "other")
	for _, m := range acct {
		if heldBy(served, m) || !heldBy(before, m) {
			t.Fatalf("over the last test's view, the tracker files %s as served in its test (%v) or not before it (%v)", m.Name, heldBy(served, m), heldBy(before, m))
		}
	}

	// A pool view between: the next test's account starts empty, and a
	// reusable recording it is served again is its own.
	mm.ServeExactPool("k", []string{"pool"})
	equalNames(t, "the pool view after T1", mm.ServedInOpenTest())
	tr.Begin()
	mm.ServeExactTest("T2", nil)
	equalNames(t, "T2 just begun", mm.ServedInOpenTest())
	mm.UpdateUnFilteredMock(hs, hs)
	equalNames(t, "T2's account", mm.ServedInOpenTest(), "handshake")
}

// A serve is filed in the store's account after the observer is told of it,
// and in the view that was installed when the observer was told: the
// observer never finds the serve it is told of in the account, and a test's
// end and the next one's begin landing while the observer is told leave the
// serve out of the next test's account. Both kinds of door: one handed the
// parser's mock (update) and one that takes it by value (delete).
func TestTheOpenTestsAccountIsFiledAfterTheObserverInTheViewItWasToldIn(t *testing.T) {
	doors := map[string]func(mm *MockManager, m *models.Mock){
		"update": func(mm *MockManager, m *models.Mock) { mm.UpdateUnFilteredMock(m, m) },
		"delete": func(mm *MockManager, m *models.Mock) { mm.DeleteUnFilteredMock(*m) },
	}
	for name, serve := range doors {
		t.Run(name, func(t *testing.T) {
			own := exactMock("own", 0, models.LifetimePerTest)
			hs := configMock("handshake", time.Second)
			mm := stagedManager(t, []*models.Mock{own}, []*models.Mock{hs})
			var early []string
			moveOn := false
			mm.SetServedObserver(func(m *models.Mock) {
				if heldBy(mm.ServedInOpenTest(), m) {
					early = append(early, m.Name)
				}
				if moveOn {
					moveOn = false
					mm.ServeExactPool("k", nil)
					mm.ServeExactTest("T2", nil)
				}
			})
			mm.ServeExactTest("T1", []*models.Mock{own})
			if !mm.DeleteFilteredMock(*own) {
				t.Fatal("the open test's own mock was refused")
			}
			serve(mm, hs)
			if len(early) != 0 {
				t.Fatalf("the account held %v before the observer was told of them", early)
			}
			equalNames(t, "T1's account", mm.ServedInOpenTest(), "own", "handshake")

			// T1 ends and T2 begins while the observer is told of a serve
			// read in T1's view: the serve is not T2's.
			moveOn = true
			serve(mm, hs)
			if moveOn {
				t.Fatal("the observer was not told of the serve")
			}
			equalNames(t, "T2's account after a serve read in T1's view", mm.ServedInOpenTest())
		})
	}
}

// With no served observer (the switch off) the store files nothing, an
// observer taken away included; a scoped view forwards the store's account,
// and one over a store that keeps none says none.
func TestTheOpenTestsAccountIsKeptOnlyWithAnObserverAndForwarded(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	hs := configMock("handshake", time.Second)
	mm := stagedManager(t, []*models.Mock{own}, []*models.Mock{hs})
	mm.ServeExactTest("T1", []*models.Mock{own})
	mm.UpdateUnFilteredMock(hs, hs)
	if !mm.DeleteFilteredMock(*own) {
		t.Fatal("the open test's own mock was refused")
	}
	if got := mm.ServedInOpenTest(); got != nil {
		t.Fatalf("with no observer the store filed %v", exactNames(got))
	}

	mm.SetServedObserver(func(*models.Mock) {})
	mm.UpdateUnFilteredMock(hs, hs)
	mm.SetServedObserver(nil)
	mm.DeleteFilteredMock(*own)
	mm.MarkMockAsUsed(models.Mock{Name: "other", Kind: models.HTTP})
	equalNames(t, "an observer taken away", mm.ServedInOpenTest(), "handshake")

	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	sr, ok := view.(integrations.ExactServedReader)
	if !ok {
		t.Fatal("a scoped view does not forward the store's account")
	}
	equalNames(t, "a scoped view's account", sr.ServedInOpenTest(), "handshake")
	type noAccount struct{ integrations.MockMemDb }
	var bare integrations.MockMemDb = &scopedMockDb{MockMemDb: noAccount{mm}}
	if got := bare.(integrations.ExactServedReader).ServedInOpenTest(); got != nil {
		t.Fatalf("a scoped view over a store that keeps no account says %v", exactNames(got))
	}
}

// With no served observer (the switch off) a door that takes its mock by value
// keeps it off the heap, an exact view installed or not: the observer's copy,
// and the account's, are made only when there is an observer.
func TestWithNoObserverADoorByValueAllocatesNothingForTheAccount(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own}, nil)
	mk := *own
	if n := testing.AllocsPerRun(100, func() { mm.observeServedCopy(mk) }); n != 0 {
		t.Fatalf("with no view and no observer a by-value door allocates %v per serve", n)
	}
	mm.ServeExactTest("T1", []*models.Mock{own})
	if n := testing.AllocsPerRun(100, func() { mm.observeServedCopy(mk) }); n != 0 {
		t.Fatalf("with a test's view and no observer a by-value door allocates %v per serve", n)
	}
}

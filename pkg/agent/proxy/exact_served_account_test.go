package proxy

import (
	"runtime"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// account is r's account of what its open test was served, without its
// stamp.
func account(r integrations.ExactServedReader) []*models.Mock {
	_, l := r.ServedInOpenTest()
	return l
}

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
	equalNames(t, "no test open", account(mm))

	tr.Begin()
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	equalNames(t, "a test just begun: the pool's serve is not its", account(mm))

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
	equalNames(t, "the open test's account", account(mm), "own", "handshake", "set-config", "w-boot", "other")
	equalNames(t, "the open test's own answered", mm.ConsumedTestMocks(), "own")
	_, _, served, _ := tr.Test()
	for _, m := range account(mm) {
		if !heldBy(served, m) {
			t.Fatalf("the store's account holds %s, which the tracker did not hear of in its running test", m.Name)
		}
	}

	// The next test begun over T1's view: the store's account is T1's, and
	// the tracker files every mock of it before its running test.
	tr.End()
	tr.Begin()
	_, _, served, before := tr.Test()
	acct := account(mm)
	equalNames(t, "begun over the last test's view", acct, "own", "handshake", "set-config", "w-boot", "other")
	for _, m := range acct {
		if heldBy(served, m) || !heldBy(before, m) {
			t.Fatalf("over the last test's view, the tracker files %s as served in its test (%v) or not before it (%v)", m.Name, heldBy(served, m), heldBy(before, m))
		}
	}

	// A pool view between: the next test's account starts empty, and a
	// reusable recording it is served again is its own.
	mm.ServeExactPool("k", []string{"pool"})
	equalNames(t, "the pool view after T1", account(mm))
	tr.Begin()
	mm.ServeExactTest("T2", nil)
	equalNames(t, "T2 just begun", account(mm))
	mm.UpdateUnFilteredMock(hs, hs)
	equalNames(t, "T2's account", account(mm), "handshake")
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
				if heldBy(account(mm), m) {
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
			equalNames(t, "T1's account", account(mm), "own", "handshake")

			// T1 ends and T2 begins while the observer is told of a serve
			// read in T1's view: the serve is not T2's.
			moveOn = true
			serve(mm, hs)
			if moveOn {
				t.Fatal("the observer was not told of the serve")
			}
			equalNames(t, "T2's account after a serve read in T1's view", account(mm))
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
	if got := account(mm); got != nil {
		t.Fatalf("with no observer the store filed %v", exactNames(got))
	}

	mm.SetServedObserver(func(*models.Mock) {})
	mm.UpdateUnFilteredMock(hs, hs)
	mm.SetServedObserver(nil)
	mm.DeleteFilteredMock(*own)
	mm.MarkMockAsUsed(models.Mock{Name: "other", Kind: models.HTTP})
	equalNames(t, "an observer taken away", account(mm), "handshake")

	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	sr, ok := view.(integrations.ExactServedReader)
	if !ok {
		t.Fatal("a scoped view does not forward the store's account")
	}
	equalNames(t, "a scoped view's account", account(sr), "handshake")
	type noAccount struct{ integrations.MockMemDb }
	var bare integrations.MockMemDb = &scopedMockDb{MockMemDb: noAccount{mm}}
	if got := account(bare.(integrations.ExactServedReader)); got != nil {
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

// oneTest is the read ExactServedReader's contract gives a learn step: the
// tracker's Epoch, the store's stamp and account, the tracker's Test; the
// reads are of one test when the stamp is not 0 and equals both epochs.
func oneTest(tr *learn.Tracker, r integrations.ExactServedReader) (bool, []*models.Mock, []*models.Mock) {
	e0 := tr.Epoch()
	stamp, acct := r.ServedInOpenTest()
	e1, _, served, _ := tr.Test()
	return stamp != 0 && stamp == e0 && stamp == e1, acct, served
}

// stampOf is the stamp of r's open test's view.
func stampOf(r integrations.ExactServedReader) uint64 {
	n, _ := r.ServedInOpenTest()
	return n
}

// Each test's view carries the learn tracker's open test as it was installed
// (ServedInOpenTest's stamp), so a learn step compares test numbers instead
// of inferring the test from what the two accounts hold. A view installed
// with the tracker's test begun carries its number, through the plain and the
// variables-opening door, and keeps it when that test's view is installed
// again with no other test's between; a pool view carries 0. A view the
// tracker began no test for alone carries 0: installed before any Begin,
// after End, after NewSet, and every view installed in a tracker test after
// a second test's view was, the first test's again included. A Begin that lands while the last
// test's view still serves leaves that view's stamp behind the tracker's
// epoch, so the reads are of no one test even where the account holds only
// what the tracker heard of. With no tracker every view carries 0, and a
// scoped view forwards the stamp.
func TestATestsViewIsStampedWithTheTrackersOpenTest(t *testing.T) {
	own1 := exactMock("own-1", 0, models.LifetimePerTest)
	own2 := exactMock("own-2", time.Second, models.LifetimePerTest)
	hs := configMock("handshake", 2*time.Second)
	pool := httpCall("pool", 3*time.Second, "/pool")
	mm := stagedManager(t, []*models.Mock{own1, own2}, []*models.Mock{hs, pool})
	tr := learn.NewTracker()
	mm.SetServedObserver(tr.ServedMock)
	mm.SetOpenTestEpoch(tr.OpenEpoch)
	stamp := func(step string, want uint64) {
		t.Helper()
		if got := stampOf(mm); got != want {
			t.Fatalf("%s: the open test's view carries %d; want %d", step, got, want)
		}
	}

	mm.ServeExactTest("T0", []*models.Mock{own1})
	stamp("a test's view before the tracker's first Begin", 0)
	mm.ServeExactPool("k", []string{"pool"})
	stamp("a pool view", 0)

	tr.Begin()
	e1 := tr.Epoch()
	mm.ServeExactTest("T1", []*models.Mock{own1})
	stamp("T1's view, installed in the tracker's T1", e1)
	mm.UpdateUnFilteredMock(hs, hs)
	if ok, acct, served := oneTest(tr, mm); !ok {
		t.Fatal("T1's reads, with nothing moved between them, are of no one test")
	} else {
		equalNames(t, "T1's account", acct, "handshake")
		if !heldBy(served, hs) {
			t.Fatal("the tracker did not hear of T1's serve")
		}
	}
	mm.OpenExactTest("T1", []*models.Mock{own1}, nil)
	stamp("T1's view installed again, through the variables-opening door, in the same tracker test", e1)
	mm.ServeExactTest("T2", []*models.Mock{own2})
	stamp("T2's view installed in the tracker's T1, with no Begin of its own", 0)
	mm.ServeExactTest("T1", []*models.Mock{own1})
	stamp("T1's view installed again after T2's in the same tracker test, which filed T2's serves", 0)
	mm.ServeExactTest("T2", []*models.Mock{own2})
	stamp("T2's view installed again in the tracker's T1", 0)
	mm.ServeExactTest("T2", []*models.Mock{own2})
	stamp("T2's view installed a third time, right after its second, in the tracker's T1", 0)
	if ok, _, _ := oneTest(tr, mm); ok {
		t.Fatal("T2's view, which the tracker began no test for, read as the tracker's running test")
	}

	tr.End()
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("T2", []*models.Mock{own2})
	stamp("T2's view installed after the tracker's End, before its Begin", 0)

	tr.Begin()
	e2 := tr.Epoch()
	mm.ServeExactTest("T2", []*models.Mock{own2})
	stamp("T2's view, installed in the tracker's T2", e2)
	tr.Begin() // the next test begins over T2's view
	mm.UpdateUnFilteredMock(hs, hs)
	ok, acct, served := oneTest(tr, mm)
	if ok {
		t.Fatal("a test begun over the last one's view read as one test with it")
	}
	equalNames(t, "T2's view after the next Begin", acct, "handshake")
	if !heldBy(served, hs) {
		t.Fatal("the tracker did not file the serve in its running test: the account alone cannot show the gap")
	}

	tr.NewSet()
	mm.ServeExactTest("T3", []*models.Mock{own1})
	stamp("a test's view installed after NewSet, before the set's first Begin", 0)

	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	tr.Begin()
	e4 := tr.Epoch()
	mm.ServeExactTest("T3", []*models.Mock{own1})
	if got := stampOf(view.(integrations.ExactServedReader)); got != e4 {
		t.Fatalf("a scoped view forwards the stamp %d; want %d", got, e4)
	}

	bare := stagedManager(t, []*models.Mock{own1}, nil)
	bare.SetServedObserver(tr.ServedMock)
	tr.Begin()
	bare.ServeExactTest("T1", []*models.Mock{own1})
	if got := stampOf(bare); got != 0 {
		t.Fatalf("with no tracker's open test to read, a test's view carries %d", got)
	}
	bare.SetOpenTestEpoch(tr.OpenEpoch)
	bare.SetOpenTestEpoch(nil)
	bare.ServeExactTest("T1", []*models.Mock{own1})
	if got := stampOf(bare); got != 0 {
		t.Fatalf("with the tracker's open test taken away, a test's view carries %d", got)
	}
}

// Under the proven switch the proxy stamps each test's view with its own
// learn tracker's open test, the one its capture window begins
// (setMockManager): not its epoch alone, so a view installed after the
// window's test ended, after a new set, or after a window taken back
// (WithdrawTestErrorCapture) carries 0. With the switch off it gives the
// store no tracker, and every view carries 0.
func TestTheProxyStampsATestsViewWithItsTrackersOpenTest(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[on])
			own := exactMock("own", 0, models.LifetimePerTest)
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4), learnTracker: learn.NewTracker()}
			mm := stagedManager(t, []*models.Mock{own}, nil)
			p.setMockManager(mm)
			p.BeginTestErrorCapture()
			mm.ServeExactTest("T1", []*models.Mock{own})
			want := uint64(0)
			if on {
				want = p.learnTracker.Epoch()
				if want == 0 {
					t.Fatal("the capture window began no tracker test")
				}
			}
			if got := stampOf(mm); got != want {
				t.Fatalf("the test's view carries %d; want %d", got, want)
			}
			p.EndTestErrorCapture()
			mm.ServeExactTest("T1", []*models.Mock{own})
			if got := stampOf(mm); got != 0 {
				t.Fatalf("the test's view installed again after its window closed carries %d", got)
			}
			p.ContinueTestErrorCapture()
			p.WithdrawTestErrorCapture()
			mm.ServeExactTest("T2", []*models.Mock{own})
			if got := stampOf(mm); got != 0 {
				t.Fatalf("a view installed after its window was taken back carries %d", got)
			}
			p.learnTracker.NewSet()
			mm.ServeExactTest("T3", []*models.Mock{own})
			if got := stampOf(mm); got != 0 {
				t.Fatalf("a view installed after a new set, before its first test began, carries %d", got)
			}
		})
	}
}

// A test's view is stamped and starts serving under one lock (installMu),
// on both install doors: the install's read of the tracker's open test and
// its view's publication are made under it, so no other test's view is
// installed between the two, and the stamps' order is the order the views
// serve in. Stamped by a swap with no lock around the view's publication, an
// install that read tracker test 8 could publish after another test's view
// was stamped in 9, or after a view whose stamp came later, and a view of
// tracker test 9 then carried 9 though another test's view served in it.
// The row reads each test's view at its publication itself
// (exactState.beforePublish, just before the swap) and checks that the
// stamp it is published with is the stamp it carries once the install
// returns, and that the first test's view still carries its stamp once the
// next install, which closes it, returns. So a view published before its
// stamp is set fails it, and so does a write to the stamp, once the view is
// published, that is still there when its install or the next returns. A
// write that is undone before then, one of the value the view already
// carries, or one made later passes this row: it is the next row's to
// catch, and that row may miss it. At the same seam the row checks the rest
// of what installExact's doc says a view is published with: a test's view's
// test and mocks, and what installExact gives every view from the manager
// (the outside answers and the staging epoch, checked on a pool view too,
// and the worker set, seeded here, on the tests' views). Any of those set
// after the swap fails it.
// A view published before its stamp was set would be read with no stamp by a
// learn step reading ServedInOpenTest beside the install: a learn step that
// refuses a 0 would drop its calls at random, and the stamp's write would
// race that read in a switch-on replay.
func TestATestsViewIsStampedAndServedUnderOneLock(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	for _, door := range []struct {
		name    string
		install func(mm *MockManager, test string)
	}{
		{name: "ServeExactTest", install: func(mm *MockManager, test string) { mm.ServeExactTest(test, []*models.Mock{own}) }},
		{name: "OpenExactTest", install: func(mm *MockManager, test string) { mm.OpenExactTest(test, []*models.Mock{own}, nil) }},
	} {
		t.Run(door.name, func(t *testing.T) {
			// held says whether an install holds the lock, as its view is
			// published and as it reads the tracker.
			mm := NewMockManager(nil, nil, zap.NewNop())
			held := func() bool {
				if mm.exact.installMu.TryLock() {
					mm.exact.installMu.Unlock()
					return false
				}
				return true
			}
			// published is the test and the stamp of each test's view as
			// installExact is about to publish it, read on the install's own
			// goroutine: the view a reader loading it first would see.
			type publication struct {
				test  string
				stamp uint64
				view  *exactView
			}
			var published []publication
			pools := 0
			mm.exact.beforePublish = func(v *exactView) {
				// What installExact gives every view from the manager is
				// there as it is published: with no outside answers a reader
				// would show again a recording consumed outside every test.
				if v.outside != &mm.exact.outside || v.epoch != &mm.stagingEpoch {
					t.Errorf("a view (test %q) was about to be published without the manager's outside answers and staging epoch: a reader loading it first would show again what calls outside every test consumed", v.test)
				}
				if v.test == "" {
					pools++
					return // a pool view: never stamped
				}
				if mm.exact.view.Load() == v {
					t.Errorf("%s's view was already published as the seam saw it: the row reads a view as it is published only if beforePublish runs before the swap", v.test)
				}
				// The worker set the manager holds (seeded below), and the
				// test's own mocks, are there as it is published: with no
				// worker set a reader would refuse a worker-scope mock.
				if _, ok := v.worker["w-boot"]; !ok || len(v.worker) != 1 {
					t.Errorf("%s's view was about to be published with the worker set %v; want the manager's, [w-boot]: a reader loading it first would refuse its worker-scope mocks", v.test, v.worker)
				}
				if len(v.owned) != 1 || v.owned[0].Name != own.Name {
					names := make([]string, 0, len(v.owned))
					for _, mk := range v.owned {
						names = append(names, mk.Name)
					}
					t.Errorf("%s's view was about to be published with the test mocks %v; want its one, %s", v.test, names, own.Name)
				}
				published = append(published, publication{test: v.test, stamp: v.stamp, view: v})
				if !held() {
					t.Errorf("view %d was published with no install lock held: another test's view could be stamped before it and published after it", len(published))
				}
			}
			// publishedCarrying checks that the last view published is
			// test's, and that it carried stamp, the stamp it carries once
			// installed, as it was published.
			publishedCarrying := func(test string, stamp uint64) {
				t.Helper()
				n := len(published)
				if n == 0 || published[n-1].test != test {
					t.Fatalf("the test views were published as %+v, and %d views with no test; want %s's last: a test's view published before its test is set is taken for a pool view", published, pools, test)
				}
				if got := published[n-1].stamp; got != stamp {
					t.Fatalf("%s's view was published carrying %d and carries %d once installed: its stamp was set after a reader could load the view", test, got, stamp)
				}
			}
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow([]*models.Mock{own}, nil, models.BaseTime, time.Now())
			mm.ServeExactWorker([]string{"w-boot"})
			tr := learn.NewTracker()
			reads := 0
			mm.SetOpenTestEpoch(func() uint64 {
				reads++
				if !held() {
					t.Errorf("install %d read the tracker's open test with no install lock held: another test's view could be installed before its view is published", reads)
				}
				return tr.OpenEpoch()
			})
			tr.Begin()
			door.install(mm, "C")
			if got := stampOf(mm); got != tr.Epoch() {
				t.Fatalf("C's view, the first installed in the tracker's test %d, carries %d", tr.Epoch(), got)
			}
			publishedCarrying("C", tr.Epoch())
			door.install(mm, "E")
			if got := stampOf(mm); got != 0 {
				t.Fatalf("E's view, installed after C's in the tracker's test %d, carries %d; want 0", tr.Epoch(), got)
			}
			publishedCarrying("E", 0)
			if got := published[0].view.stamp; got != tr.Epoch() {
				t.Fatalf("C's view carries %d once E's install, which closed it, returned; want %d, the stamp it was published with", got, tr.Epoch())
			}
			if reads != 2 || len(published) != 2 || pools != 0 {
				t.Fatalf("the two installs read the tracker's open test %d times, and published %d test views and %d with no test; want one read and one test view each, 2, 2 and 0", reads, len(published), pools)
			}
			// A pool view is published with the manager's outside answers
			// and staging epoch too (checked at the seam).
			mm.ServeExactPool("k", nil)
			if pools != 1 || len(published) != 2 {
				t.Fatalf("the pool call published %d views with no test, and the test views are now %d; want 1 and 2", pools, len(published))
			}
		})
	}
}

// A learn step reads ServedInOpenTest while the replay installs tests' views,
// on both doors: in a switch-on replay the two run at once. It never reads a
// test's view without the stamp that view carries once installed: first
// beside installs of one test's view, each carrying the tracker's open test,
// then beside installs of two tests' views in that tracker test, each
// carrying 0. It is the guard for the writes the row above cannot see: a
// write to the stamp once the view is published that is undone before the
// install returns, one of the value the view already carries, or one made
// later. The second half is the one that can see a write of the tracker's
// open test onto a view that carries 0, which a learn step would take for
// the test's own stamp: a false green, not a refusal. It catches such a
// write only by the race report or by chance. Under -race, a write under no
// lock this read also takes races the read, and -race reports it. Otherwise
// it sees a write only if one of its reads falls between the write and its
// undoing, and a write of the value the view already carries not at all.
func TestAReaderBesideTheInstallsAlwaysReadsTheTestsViewStamped(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, models.BaseTime, time.Now())
	tr := learn.NewTracker()
	mm.SetOpenTestEpoch(tr.OpenEpoch)
	tr.Begin()
	epoch := tr.Epoch()
	installOf := func(k int, test string) {
		if k%2 == 0 {
			mm.ServeExactTest(test, []*models.Mock{own})
		} else {
			mm.OpenExactTest(test, []*models.Mock{own}, nil)
		}
	}
	// beside reads ServedInOpenTest on another goroutine while the installs
	// run, each of which leaves the view carrying want, and fails if a read
	// is of another stamp. The installs start once the reader reads, so the
	// two overlap; the reader is stopped and joined before it returns.
	beside := func(views string, want uint64, install func(k int)) {
		t.Helper()
		type tally struct {
			reads, bad int
			last       uint64
		}
		stop, reading := make(chan struct{}), make(chan struct{})
		done := make(chan tally, 1)
		go func() {
			var n tally
			for {
				select {
				case <-stop:
					done <- n
					return
				default:
				}
				got, _ := mm.ServedInOpenTest()
				if n.reads++; n.reads == 1 {
					close(reading)
				}
				if got != want {
					n.bad, n.last = n.bad+1, got
				}
				runtime.Gosched()
			}
		}()
		<-reading
		const installs = 2000
		for k := 1; k <= installs; k++ {
			install(k)
			if got := stampOf(mm); got != want {
				t.Errorf("install %d of %s left the view carrying %d; want %d", k, views, got, want)
				break
			}
			runtime.Gosched()
		}
		close(stop)
		n := <-done
		if t.Failed() {
			return
		}
		if n.bad != 0 {
			t.Fatalf("of %d reads beside %d installs of %s, each of which left the view carrying %d, %d read another stamp (the last %d): a stamp was written once its view could be read", n.reads, installs, views, want, n.bad, n.last)
		}
	}
	// Every view installed first is C's, in the one tracker test, so each
	// carries its open test (a test's view installed again in its tracker
	// test keeps it).
	installOf(0, "C")
	if got, _ := mm.ServedInOpenTest(); got != epoch {
		t.Fatalf("precondition: C's view, the first installed in the tracker's test %d, carries %d", epoch, got)
	}
	beside("C's view", epoch, func(k int) { installOf(k, "C") })
	if t.Failed() {
		return
	}
	// E's view installed in the same tracker test: every view installed in
	// it from then on, C's or E's, carries 0.
	installOf(0, "E")
	if got, _ := mm.ServedInOpenTest(); got != 0 {
		t.Fatalf("precondition: E's view, installed after C's in the tracker's test %d, carries %d; want 0", epoch, got)
	}
	beside("C's and E's views", 0, func(k int) {
		test := "C"
		if (k/2)%2 == 1 {
			test = "E"
		}
		installOf(k, test)
	})
}

package proxy

import (
	"runtime"
	"slices"
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
// on all four install doors: the install's read of the tracker's open test and
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
// stamp is set fails it, and so does a write of another value to the stamp,
// once the view is published, that is still there when its install or the
// next returns. A write of the value the view already carries, one undone
// before then, or one made after the install returns passes this row: the
// next row (TestAReaderBesideTheInstallsAlwaysReadsTheTestsViewStamped)
// reads each view at installExact's end, and sees a write of another value
// still there then every run, unless it was made under the view's lock and
// that lock is still held there, which hangs that row until go test's
// timeout fails it; the rest it sees only by chance or by the race report.
// At the same seam the row checks the rest of what installExact's doc says
// a view is published with, but for the calls the gap answered and those in
// flight, which TestATestsViewIsPublishedWithTheGapsAnswersAndItsCallsInFlight
// checks: a test's view's test,
// mocks and claims, and on the concurrent doors, given one shared recording,
// that the view marks it shared and its copy says so
// (TestModeInfo.Concurrent); a pool view's pool names and ahead mock; and
// what installExact gives every view from the manager (the outside answers
// and the staging epoch, checked on the pool view too, and the worker set,
// seeded here, on the tests' views). Any of those set after the swap fails
// it. A concurrent view published without its marks would be read with its
// shared recording taken for the test's own: answered with no claim, so a
// second time if another test claimed it in the staging, and named by no
// ExactTakenCandidates. A copy published not saying it is shared would be
// one a parser's learn step learns against.
// A view published before its stamp was set would be read with no stamp by a
// learn step reading ServedInOpenTest beside the install: a learn step that
// refuses a 0 would drop its calls at random, and the stamp's write would
// race that read in a switch-on replay.
func TestATestsViewIsStampedAndServedUnderOneLock(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	shared := exactMock("shared", 0, models.LifetimePerTest)
	ahead := exactMock("ahead", 0, models.LifetimePerTest)
	for _, door := range []struct {
		name string
		// concurrent: the door gives the test shared, after own, as a
		// recording it shares with the tests beside it.
		concurrent bool
		install    func(mm *MockManager, test string)
	}{
		{name: "ServeExactTest", install: func(mm *MockManager, test string) { mm.ServeExactTest(test, []*models.Mock{own}) }},
		{name: "OpenExactTest", install: func(mm *MockManager, test string) { mm.OpenExactTest(test, []*models.Mock{own}, nil) }},
		{name: "ServeExactTestConcurrent", concurrent: true, install: func(mm *MockManager, test string) {
			mm.ServeExactTestConcurrent(test, []*models.Mock{own}, []*models.Mock{shared})
		}},
		{name: "OpenExactTestConcurrent", concurrent: true, install: func(mm *MockManager, test string) {
			mm.OpenExactTestConcurrent(test, []*models.Mock{own}, []*models.Mock{shared}, nil)
		}},
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
					// A pool view (never stamped): its pool names and the
					// next test's mock in flight are there as it is published.
					// With neither a reader would refuse what the pool and
					// the gap answer.
					pools++
					if _, ok := v.pool["p-name"]; !ok || len(v.pool) != 1 {
						t.Errorf("the pool view was about to be published with the pool names %v; want [p-name]: a reader loading it first would refuse the pool's mocks", v.pool)
					}
					if got := exactNames(v.owned); !slices.Equal(got, []string{ahead.Name}) {
						t.Errorf("the pool view was about to be published with the next test's mocks in flight %v; want [%s]: a reader loading it first would refuse the call in the gap", got, ahead.Name)
					}
					return
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
				want := []string{own.Name}
				if door.concurrent {
					want = append(want, shared.Name)
				}
				if got := exactNames(v.owned); !slices.Equal(got, want) {
					t.Errorf("%s's view was about to be published with the mocks %v; want %v", v.test, got, want)
				}
				// Which of them are shared, and the claims a shared one is
				// answered under, are there as it is published: a reader
				// loading it first would take a shared recording with no
				// mark for the test's own.
				for i, mk := range v.owned {
					if isShared := mk.Name == shared.Name; v.isCand(i) != isShared || mk.TestModeInfo.Concurrent != isShared {
						t.Errorf("%s's view was about to be published with %s marked shared %v and its copy saying so %v; want %v for both: a reader loading it first would take a shared recording for the test's own, and answer it with no claim", v.test, mk.Name, v.isCand(i), mk.TestModeInfo.Concurrent, isShared)
					}
				}
				if v.claims != &mm.exact.claims {
					t.Errorf("%s's view was about to be published without the manager's claims: a reader loading it first could not see a shared recording another test answered with", v.test)
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
			store := []*models.Mock{own}
			if door.concurrent {
				store = append(store, shared)
			}
			mm.SetMocksWithWindow(store, nil, models.BaseTime, time.Now())
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
			// and staging epoch too, and with its pool names and ahead mock
			// (checked at the seam).
			mm.ServeExactPool("k", []string{"p-name"}, ahead)
			if pools != 1 || len(published) != 2 {
				t.Fatalf("the pool call published %d views with no test, and the test views are now %d; want 1 and 2", pools, len(published))
			}
		})
	}
}

// A learn step reads ServedInOpenTest while the replay installs tests' views,
// on all four doors: in a switch-on replay the two run at once. It never
// reads a test's view without the stamp that view carries once installed:
// first beside installs of one test's view, each carrying the tracker's open
// test, then beside installs of two tests' views in that tracker test, each
// carrying 0. It is the guard for the writes the row above cannot see: a
// write to the stamp once the view is published that is undone before the
// install returns, or one made after the install returns. The second half
// is the one that can see a write of the tracker's open test onto a view
// that carries 0, which a learn step would take for the test's own stamp: a
// false green, not a refusal.
// Each install parks a reader at installExact's end (exactState.afterPublish:
// the view published, the view before it closed, the revision and the
// window signal moved, each of which the seam checks, so it cannot be called
// earlier) and has it read there, on its own goroutine, while the install
// waits: a write of another value to the stamp made after the swap and still
// there then, under no lock or under one released by then, fails the row
// every run. One made under the view's lock that is still held there holds
// the parked read too: the row does not fail then, it hangs until go test's
// timeout fails it.
// What that read cannot see: a write of the value the view already carries;
// a write undone before installExact's end; and a write made after it and
// undone before the door returns (after the seam the install still logs,
// and the door releases the install lock, finishes the variables' opening
// on the open doors, and flags the calls carried in). No row can read at
// every point of an install: a write undone between two reads leaves nothing
// for a later read to see, so only a read placed between the two sees it,
// and the rows read just before the swap (the row above), at installExact's
// end and once the door returns. So a second reader reads freely beside the
// installs, on a goroutine of its own that the parked reads never wait on,
// and sees such a write only by chance, or under -race by the race report
// when it was made under no lock this read also takes.
func TestAReaderBesideTheInstallsAlwaysReadsTheTestsViewStamped(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	shared := exactMock("shared", 0, models.LifetimePerTest)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow([]*models.Mock{own, shared}, nil, models.BaseTime, time.Now())
	tr := learn.NewTracker()
	mm.SetOpenTestEpoch(tr.OpenEpoch)
	tr.Begin()
	epoch := tr.Epoch()
	// installOf installs test's view through door k%4: the two plain doors,
	// then the two concurrent ones, each given shared.
	installOf := func(k int, test string) {
		owned, cands := []*models.Mock{own}, []*models.Mock{shared}
		switch k % 4 {
		case 0:
			mm.ServeExactTest(test, owned)
		case 1:
			mm.OpenExactTest(test, owned, nil)
		case 2:
			mm.ServeExactTestConcurrent(test, owned, cands)
		default:
			mm.OpenExactTestConcurrent(test, owned, cands, nil)
		}
	}
	// park, while beside runs, is the parked reader's: each install's end
	// hands it a reply channel and waits for the stamp it read. parked counts
	// the installs that did, and parkedRead is the last stamp read. All three
	// are the installs' goroutine's (the test's).
	var park chan chan uint64
	parked, parkedRead := 0, uint64(0)
	// before is what the view before each install's view was, and the window
	// signal and revision a reader woken by the install waits on, as
	// installExact is about to publish the view: at its end the seam checks
	// the view before is closed, the signal fired and the revision moved, so
	// the parked read is placed where a reader the signal woke reads.
	var before struct {
		view *exactView
		sig  <-chan struct{}
		rev  uint64
	}
	mm.exact.beforePublish = func(*exactView) {
		before.view, before.sig, before.rev = mm.exact.view.Load(), mm.WindowChanged(), mm.Revision()
	}
	mm.exact.afterPublish = func(v *exactView) {
		if park == nil {
			return
		}
		if mm.exact.view.Load() != v {
			t.Errorf("%s's view was not the view served at installExact's end", v.test)
		}
		closed := false
		if before.view != nil {
			before.view.mu.Lock()
			closed = before.view.closed
			before.view.mu.Unlock()
		}
		fired := false
		select {
		case <-before.sig:
			fired = true
		default:
		}
		if !closed || !fired || mm.Revision() == before.rev {
			t.Errorf("at the seam called as %s's view's install ends, the view before it was closed %v, the window signal fired %v and the revision moved %v; want all three: the seam reads before installExact's end, so a write still there at its end can pass the parked read", v.test, closed, fired, mm.Revision() != before.rev)
		}
		reply := make(chan uint64)
		park <- reply
		parkedRead = <-reply
		parked++
	}
	// beside runs the installs, each of which leaves the view carrying want,
	// with two readers of ServedInOpenTest on goroutines of their own: the
	// parked one, which reads at each install's end, and the free one, which
	// reads in a loop and fails the row if a read is of another stamp. The
	// installs start once the free reader reads, so the two overlap; both
	// readers are stopped and joined before it returns.
	beside := func(views string, want uint64, install func(k int)) {
		t.Helper()
		type tally struct {
			reads, bad int
			last       uint64
		}
		stop, reading := make(chan struct{}), make(chan struct{})
		park, parked = make(chan chan uint64), 0
		done := make(chan tally, 1)
		parkerDone := make(chan struct{})
		go func(park <-chan chan uint64) {
			defer close(parkerDone)
			for {
				select {
				case <-stop:
					return
				case reply := <-park:
					got, _ := mm.ServedInOpenTest()
					reply <- got
				}
			}
		}(park)
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
			if t.Failed() {
				break
			}
			if parked != k || parkedRead != want {
				t.Errorf("install %d of %s: the reader parked at installExact's end read %d (%d of %d installs parked it); want %d, the stamp each view carries once installed: a stamp was written once its view could be read", k, views, parkedRead, parked, k, want)
				break
			}
			if got := stampOf(mm); got != want {
				t.Errorf("install %d of %s left the view carrying %d; want %d", k, views, got, want)
				break
			}
			runtime.Gosched()
		}
		park = nil
		close(stop)
		n := <-done
		<-parkerDone
		if t.Failed() {
			return
		}
		if n.bad != 0 {
			t.Fatalf("of %d free reads beside %d installs of %s, each of which left the view carrying %d, %d read another stamp (the last %d): a stamp was written once its view could be read", n.reads, installs, views, want, n.bad, n.last)
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
	// Each door installs C's view and E's in turn.
	beside("C's and E's views", 0, func(k int) {
		test := "C"
		if (k/4)%2 == 1 {
			test = "E"
		}
		installOf(k, test)
	})
}

// A view is published with what its door reads from the pool view it
// closes, on all four test doors and on the pool door, for four shapes of
// the pool view's ahead: a test's two calls in flight, one answered in the
// gap; two calls, none answered; one call, not answered; one call, answered.
// The pool view also serves a pool-slice recording. A test's view is
// published with the answered call, if any, consumed, in the order it was
// answered and marked carried, with no other call marked answered, and with
// every one of the calls marked in flight (the pool view served them all as
// the test's calls in flight, answered or not). A pool view that replaces
// it, naming the calls again, is published with the answered call kept
// consumed, in order and with the copy it answered with. A view published
// without its consumed marks would answer that recording a second time to a
// reader that loads it first; a test's view published without its in-flight
// marks would have a learn step take the test's call in flight for the
// pool's (InFlightIntoOpenTest false), so two reads in the window where a
// test's calls in flight arrive would disagree. The seam before the swap
// checks the marks as the view is published. The row also checks what the
// marks rest on: no consume of the pool view is served after the door read
// its answers. A parser that read the pool view before the door began holds
// its copies of the pool-slice recording and of the call the gap did not
// answer, if any, and consumes them as closeAhead returns, the door's read
// done (exactState.aheadRead), and again at the seam before the swap. At
// both the pool view must be closed (read under its lock) and each consume
// refused. Served there, the call's answer would be left out of the door's
// view, built from the read: that view would answer the same recording a
// second time, and the first answer would never be flagged as the test's;
// and the pool would answer a call made as the door began a view that does
// not show the pool.
// A seam inside closeAhead's hold of the pool view's lock
// (exactState.aheadLocked) checks that it runs inside a hold of that lock
// (TryLock fails) and that the view is already closed there, so a close left
// to a later hold than the seam's fails the row. That the read is made in
// that hold too is closeAhead's code: a read moved to an earlier hold, with
// the close and the seam left together in a later one, passes, as no row
// can place a consume between two holds that correct code does not have. At
// installExact's end (exactState.afterPublish), on the install's goroutine,
// a reader answers the answered call again and must miss, and on a test's
// view must find every call in flight into the open test. Every check is
// placed, not raced, so a mark set after the swap or after the install, or
// a close left out for one of the four shapes, fails the row every run; a
// mark set under the view's lock with that lock still held at the end seam
// hangs the row instead (its reader takes the view's lock), until go test's
// timeout fails it. The concurrent doors are given a shared recording, so
// they do not run the plain doors' code.
func TestATestsViewIsPublishedWithTheGapsAnswersAndItsCallsInFlight(t *testing.T) {
	for _, fixture := range []gapFixture{
		{name: "TwoCallsOneAnswered", fetch: true, other: true, answer: true},
		{name: "TwoCallsNoneAnswered", fetch: true, other: true},
		{name: "OneCallNoneAnswered", other: true},
		{name: "OneCallAnswered", fetch: true, answer: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, door := range []gapDoor{
				{name: "ServeExactTest", install: func(mm *MockManager, owned []*models.Mock, _ *models.Mock) { mm.ServeExactTest("t2", owned) }},
				{name: "OpenExactTest", install: func(mm *MockManager, owned []*models.Mock, _ *models.Mock) { mm.OpenExactTest("t2", owned, nil) }},
				{name: "ServeExactTestConcurrent", install: func(mm *MockManager, owned []*models.Mock, shared *models.Mock) {
					mm.ServeExactTestConcurrent("t2", owned, []*models.Mock{shared})
				}},
				{name: "OpenExactTestConcurrent", install: func(mm *MockManager, owned []*models.Mock, shared *models.Mock) {
					mm.OpenExactTestConcurrent("t2", owned, []*models.Mock{shared}, nil)
				}},
				{name: "ServeExactPool", pool: true, install: func(mm *MockManager, owned []*models.Mock, _ *models.Mock) { mm.ServeExactPool("k2", nil, owned...) }},
			} {
				t.Run(door.name, func(t *testing.T) { gapRow(t, fixture, door) })
			}
		})
	}
}

// gapFixture is a shape of the pool view's ahead in
// TestATestsViewIsPublishedWithTheGapsAnswersAndItsCallsInFlight: which of
// t2's calls are in flight as the pool view begins (fetch, other), and
// whether the gap answers fetch. It never answers other.
type gapFixture struct {
	name                 string
	fetch, other, answer bool
}

// gapDoor is a door of that row. pool: it installs a pool view that names
// the calls again. install is given the calls and a shared recording, which
// the concurrent doors give the test.
type gapDoor struct {
	name    string
	pool    bool
	install func(mm *MockManager, owned []*models.Mock, shared *models.Mock)
}

// gapRow is one shape of the row on one door.
func gapRow(t *testing.T, fixture gapFixture, door gapDoor) {
	var fetch, other *models.Mock
	var calls []*models.Mock
	if fixture.fetch {
		fetch = httpCall("t2-fetch", 1e9, "/fetch")
		calls = append(calls, fetch)
	}
	if fixture.other {
		other = httpCall("t2-other", 2e9, "/other")
		calls = append(calls, other)
	}
	answer, pool := fixture.answer, door.pool
	slice := httpCall("gap-pool", 3e9, "/pool")
	shared := httpCall("t3-shared", 4e9, "/shared")
	mm := stagedManager(t, nil, append([]*models.Mock{slice, shared}, calls...))
	mm.ServeExactPool("k", []string{slice.Name}, calls...)
	if answer {
		if got := pollFirst(t, mm, fetch.Name); got != fetch.Name {
			t.Fatalf("precondition: the pool view did not answer t2's call in flight in the gap: %s", got)
		}
	}
	// held are a parser's copies of the pool-slice recording and of the
	// call the gap did not answer, read from the pool view before the door
	// begins: the pool view's own copies, so their consumes are refused
	// only if that view is closed.
	gapView := mm.exact.view.Load()
	mocks, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatalf("precondition: reading the pool view: %v", err)
	}
	var held []*models.Mock
	for _, mk := range mocks {
		if mk.Name == slice.Name || (other != nil && mk.Name == other.Name) {
			held = append(held, mk)
		}
	}
	want := 1
	if other != nil {
		want++
	}
	if len(held) != want {
		t.Fatalf("precondition: the pool view shows %d of the pool-slice recording and t2's call it did not answer; want %d", len(held), want)
	}
	for _, mk := range held {
		if mk.Name == slice.Name {
			continue
		}
		if mk.TestModeInfo.ServedIn != gapView.gen {
			t.Fatalf("precondition: the pool view does not show its own copy of t2's call in flight %s", mk.Name)
		}
	}
	// refused checks, at a seam after the door read the pool view's
	// answers, that the pool view is closed and that a consume of each held
	// copy is refused.
	refused := func(where string) {
		gapView.mu.Lock()
		closed := gapView.closed
		gapView.mu.Unlock()
		if !closed {
			t.Errorf("%s the pool view the door read its answers from was still open: a consume after the read is served and left out of the door's view", where)
		}
		for _, mk := range held {
			if !mm.DeleteUnFilteredMock(*mk) {
				continue
			}
			if mk.Name == slice.Name {
				t.Errorf("%s a parser's copy of the pool-slice recording %s, read from the pool view before the door began, was served after the door read the pool view's answers: the pool answered a call made as the door began a view that does not show it", where, mk.Name)
			} else {
				t.Errorf("%s a parser's copy of %s, read from the pool view before the door began, was served after the door read the pool view's answers: the door's view leaves it out, so it answers that recording a second time, and the first answer is never flagged as the test's", where, mk.Name)
			}
		}
	}
	// ours is the view the door installs: a test's view, or the pool view
	// of key k2.
	ours := func(v *exactView) bool {
		if pool {
			return v.test == "" && v.poolKey == "k2"
		}
		return v.test != ""
	}
	heldChecked, readChecked, seamChecked, endChecked := 0, 0, 0, 0
	mm.exact.aheadLocked = func(prev *exactView) {
		heldChecked++
		if prev != gapView {
			t.Errorf("closeAhead read a pool view other than the one the gap answered from")
			return
		}
		if prev.mu.TryLock() {
			prev.mu.Unlock()
			t.Errorf("the seam in closeAhead's hold of the pool view's lock was not inside a hold of that lock")
		}
		if !prev.closed {
			t.Errorf("the pool view was not closed by the end of the hold of its lock that read its answers: a consume between that hold and the close is served and left out of the door's view")
		}
	}
	mm.exact.aheadRead = func(prev *exactView) {
		readChecked++
		if prev != gapView {
			t.Errorf("closeAhead read a pool view other than the one the gap answered from")
			return
		}
		refused("as closeAhead returned,")
	}
	mm.exact.beforePublish = func(v *exactView) {
		if !ours(v) {
			return
		}
		seamChecked++
		if mm.exact.view.Load() == v {
			t.Errorf("the door's view was already published as the seam saw it: the row reads a view as it is published only if beforePublish runs before the swap")
		}
		// The view being replaced is the pool view the door read.
		if mm.exact.view.Load() != gapView {
			t.Errorf("the view being replaced at the seam was not the pool view the gap answered from")
		}
		refused("at the seam before the swap,")
		if answer {
			i, ok := v.index[fetch.Name]
			if !ok || !v.consumed[i] || !slices.Equal(v.order, []int{i}) {
				t.Errorf("the door's view was about to be published without the call the pool view answered in the gap marked answered (in the view %v, consumed %v, order %v): a reader loading it first answers that recording a second time", ok, ok && v.consumed[i], v.order)
			}
			if pool {
				if !ok || v.served[i] == nil {
					t.Errorf("the pool view was about to be published without the copy %s answered with: its consume, flagged as the next test opens, would say nothing of what it answered", fetch.Name)
				}
			} else if !v.carried[fetch.Name] {
				t.Errorf("t2's view was about to be published without %s marked carried: the variables its answer bound are not this test's", fetch.Name)
			}
		} else if len(v.order) != 0 || len(v.carried) != 0 || slices.Contains(v.consumed, true) {
			t.Errorf("the door's view was about to be published with a call marked answered (consumed %v, order %v, carried %v), though the gap answered none", v.consumed, v.order, v.carried)
		}
		if other != nil {
			if j, ok := v.index[other.Name]; !ok || v.consumed[j] || v.carried[other.Name] {
				t.Errorf("the door's view was about to be published with %s, which the gap did not answer, marked answered or left out (in the view %v)", other.Name, ok)
			}
		}
		if pool {
			return
		}
		for _, mk := range calls {
			if !v.inFlight[mk.Name] {
				t.Errorf("t2's view was about to be published with its in-flight marks %v; want every one of t2's calls in flight: a learn step reading it first takes %s for the pool's", v.inFlight, mk.Name)
			}
		}
	}
	mm.exact.afterPublish = func(v *exactView) {
		if !ours(v) {
			return
		}
		endChecked++
		if mm.exact.view.Load() != v {
			t.Errorf("the door's view was not the view served at installExact's end")
		}
		if answer {
			if got := pollFirst(t, mm, fetch.Name); got != "a miss" {
				t.Errorf("a reader at installExact's end answered %q with the door's view served; want a miss: the recording the pool view answered in the gap answered a second time", got)
			}
		}
		if pool {
			return
		}
		for _, mk := range calls {
			if !mm.InFlightIntoOpenTest(mk) {
				t.Errorf("a reader at installExact's end found %s not in flight into the open test: a learn step there takes it for the pool's", mk.Name)
			}
		}
	}
	door.install(mm, calls, shared)
	if heldChecked != 1 || readChecked != 1 || seamChecked != 1 || endChecked != 1 {
		t.Fatalf("the door's install ran the seam in closeAhead's hold %d times, the one at closeAhead's return %d times, the one before the swap %d times and the one at installExact's end %d times; want 1 of each", heldChecked, readChecked, seamChecked, endChecked)
	}
	// Once the door returns, the answered call still misses, and each call
	// the gap did not answer answers.
	if answer {
		if got := pollFirst(t, mm, fetch.Name); got != "a miss" {
			t.Errorf("once the door returned, a reader answered %q; want a miss", got)
		}
	} else if fetch != nil {
		if got := pollFirst(t, mm, fetch.Name); got != fetch.Name {
			t.Errorf("once the door returned, t2's call in flight answered %q; want %s", got, fetch.Name)
		}
	}
	if other != nil {
		if got := pollFirst(t, mm, other.Name); got != other.Name {
			t.Errorf("once the door returned, t2's call in flight answered %q; want %s", got, other.Name)
		}
	}
}

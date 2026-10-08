package manager

import (
	"context"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// processManagers builds n managers as a DaemonSet agent does, one per app
// (New), each wired and past the startup window, with one request in flight
// since a minute ago: the first's the oldest. The windows are closed at the
// test's end.
func processManagers(t *testing.T, n int) ([]*SyncMockManager, []*Window) {
	t.Helper()
	mgrs := make([]*SyncMockManager, n)
	wins := make([]*Window, n)
	start := time.Now().Add(-time.Minute)
	for i := range mgrs {
		m := New(nil)
		m.SetOutputChannel(make(chan *models.Mock, 16))
		m.SetMappingChannel(context.Background(), make(chan models.TestMockMapping, 16))
		m.SetFirstRequestSignaled()
		m.resolvedTestCount = models.StartupMockTestCaseWindow
		mgrs[i] = m
		wins[i] = m.OpenWindow(start.Add(time.Duration(i)*time.Millisecond), nil)
	}
	t.Cleanup(func() {
		for _, w := range wins {
			w.Close()
		}
	})
	return mgrs, wins
}

// heldInAll is what the managers hold, together, counted from each one's hold.
func heldInAll(mgrs []*SyncMockManager) (sum int64) {
	for _, m := range mgrs {
		m.mu.Lock()
		sum += m.heldBytes
		m.mu.Unlock()
	}
	return sum
}

// fillProcessHold has each of the managers' requests in flight make mocks in
// turn, which a resolve's stale cutoff would drop and the hold keeps for them,
// up to limit mocks in all or until a request is given up, and returns how
// many it added and the most the process's pool counted after any one call.
func fillProcessHold(mgrs []*SyncMockManager, wins []*Window, mk func(at time.Time, i int) *models.Mock, limit int) (added int, most int64, gaveUp bool) {
	start := wins[0].Start()
	for added < limit && !gaveUp {
		m := mgrs[added%len(mgrs)]
		m.AddMock(mk(start.Add(time.Second+time.Duration(added)*time.Microsecond), added))
		added++
		k := time.Now()
		m.ResolveRange(k, k, "test-k", true, false)
		most = max(most, processHold.total.Load())
		for _, w := range wins {
			gaveUp = gaveUp || givenUp(w)
		}
	}
	return added, most, gaveUp
}

// The managers of one process keep within MaxHeldBytes together: eight apps
// with a request in flight for a minute each, while their other calls pile
// up, hold no more than that in all after any call, and the heap it takes is
// that give or take the estimate. A budget per app would have let them hold
// eight times it. Not parallel: it reads the process's heap, and fills the
// process's own pool.
func TestHoldKeepsWithinItsBudgetAcrossManagers(t *testing.T) {
	const apps = 8
	if got := processHold.total.Load(); got != 0 {
		t.Fatalf("fixture: the process's pool counts %d bytes before the test", got)
	}
	for _, c := range sizedMocks[:2] { // small HTTP calls, and result sets of half a megabyte
		// How many mocks in all until the first request is given up.
		mgrs, wins := processManagers(t, apps)
		n, most, gaveUp := fillProcessHold(mgrs, wins, c.mk, 1<<20)
		if !gaveUp {
			t.Fatalf("%s: %d mocks held for %d requests in flight and none was given up", c.name, n, apps)
		}
		if most > MaxHeldBytes {
			t.Fatalf("%s: the process's pool counted %d bytes after a call, over the budget %d", c.name, most, MaxHeldBytes)
		}
		for _, w := range wins {
			w.Close()
		}
		if got := processHold.total.Load(); got != 0 {
			t.Fatalf("%s: with no request in flight the process's pool counts %d bytes", c.name, got)
		}

		// The process's hold one mock short of that, and the heap it takes.
		before := liveHeap()
		mgrs, wins = processManagers(t, apps)
		_, most, gaveUp = fillProcessHold(mgrs, wins, c.mk, n-1)
		heap := liveHeap() - before
		counted := heldInAll(mgrs)
		runtime.KeepAlive(mgrs)
		t.Logf("%-30s %d apps hold %5.1f MiB of heap, %5.1f MiB counted (budget %d MiB for the process)",
			c.name, apps, float64(heap)/(1<<20), float64(counted)/(1<<20), MaxHeldBytes>>20)
		if gaveUp {
			t.Fatalf("%s: a request was given up within %d mocks: the fixture did not fill the hold", c.name, n-1)
		}
		if counted != processHold.total.Load() || most > MaxHeldBytes || counted > MaxHeldBytes {
			t.Errorf("%s: the managers hold %d bytes, the pool counts %d and counted at most %d, budget %d", c.name, counted, processHold.total.Load(), most, MaxHeldBytes)
		}
		if limit := MaxHeldBytes * 3 / 2; heap > limit {
			t.Errorf("%s: the process's hold takes %d bytes of heap, want at most %d (the budget %d and the estimate's slack)", c.name, heap, limit, MaxHeldBytes)
		}
		if heap < MaxHeldBytes/2 {
			t.Errorf("%s: the process's hold is full at %d bytes of heap, under half its budget %d: the estimate reads far too large", c.name, heap, MaxHeldBytes)
		}
		for _, w := range wins {
			w.Close()
		}
	}
}

// Every manager New makes, and the package's own, share the process's budget;
// one built otherwise (a test's) has one of its own, of the same size.
func TestManagersShareTheProcesssHoldBudget(t *testing.T) {
	t.Parallel()
	if a, b := New(nil), New(nil); a.pool != processHold || b.pool != processHold {
		t.Fatal("a manager New made does not share the process's hold budget")
	}
	if Get().pool != processHold {
		t.Fatal("the package's manager does not share the process's hold budget")
	}
	if processHold.limit != MaxHeldBytes {
		t.Fatalf("the process's hold budget is %d, want MaxHeldBytes %d", processHold.limit, MaxHeldBytes)
	}
	m := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
	m.mu.Lock()
	p := m.poolLocked()
	m.mu.Unlock()
	if p == processHold || p.limit != MaxHeldBytes {
		t.Fatalf("a test's manager shares the process's pool (%v) or has a budget of %d", p == processHold, p.limit)
	}
}

// pooledManager is a test manager whose hold shares p.
func pooledManager(p *holdPool) *SyncMockManager {
	m, _ := pooledManagerOut(p)
	return m
}

// pooledManagerOut is pooledManager, with the channel its mocks are sent on.
func pooledManagerOut(p *holdPool) (*SyncMockManager, chan *models.Mock) {
	out := make(chan *models.Mock, 4096)
	m := openWindowManager(out, make(chan models.TestMockMapping, 256))
	m.pool = p
	return m, out
}

// holdOld has the manager's request in flight since start make n mocks of 32
// KiB a second after it started, which a resolve's stale cutoff holds for it.
func holdOld(m *SyncMockManager, start time.Time, n int) {
	for i := 0; i < n; i++ {
		m.AddMock(boundMockAt(start.Add(time.Second + time.Duration(i)*time.Microsecond)))
	}
	k := time.Now()
	m.ResolveRange(k, k, "test-k", true, false)
}

func givenUp(w *Window) bool {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	return w.givenUp
}

// boundUnit is the size of a boundMockAt mock.
var boundUnit = mockSize(boundMockAt(time.Time{}))

// checkPool checks that what the pool counts is what its members hold, and
// that its members are the managers that hold some.
func checkPool(t *testing.T, p *holdPool, mgrs ...*SyncMockManager) {
	t.Helper()
	var sum int64
	for _, m := range mgrs {
		m.mu.Lock()
		held, pooled, in := m.heldBytes, m.pooled, m.inPool
		oldest, published := m.oldestUnclaimedLocked(), m.oldestUnclaimed.Load()
		m.mu.Unlock()
		p.mu.Lock()
		_, member := p.members[m]
		p.mu.Unlock()
		if pooled != held || m.pooledNow.Load() != held {
			t.Fatalf("a manager holds %d bytes and published %d (%d)", held, pooled, m.pooledNow.Load())
		}
		if in != (held > 0) || member != in {
			t.Fatalf("a manager holding %d bytes is a member: %v, says it is: %v", held, member, in)
		}
		if oldest != published {
			t.Fatalf("a manager's oldest unclaimed request started at %d and it published %d", oldest, published)
		}
		sum += held
	}
	if got := p.total.Load(); got != sum {
		t.Fatalf("the pool counts %d bytes, its managers hold %d", got, sum)
	}
}

// Past the process's budget the oldest request in flight of the apps holding
// more than their share is given up first: not the process's oldest request if
// its app holds no more than its share, and not the request of the app whose
// call took the process past, if another app over its share has an older one.
func TestPoolGivesUpTheOldestRequestOfTheAppsOverTheirShare(t *testing.T) {
	t.Parallel()
	p := newHoldPool(30 * boundUnit) // a share of 10 for each of three apps
	quiet, older, busy := pooledManager(p), pooledManager(p), pooledManager(p)
	now := time.Now()
	quietWin := quiet.OpenWindow(now.Add(-90*time.Second), nil) // the process's oldest
	olderWin := older.OpenWindow(now.Add(-60*time.Second), nil)
	busyWin := busy.OpenWindow(now.Add(-30*time.Second), nil)
	defer quietWin.Close()
	defer olderWin.Close()
	defer busyWin.Close()

	holdOld(quiet, quietWin.Start(), 2)
	holdOld(older, olderWin.Start(), 12)
	holdOld(busy, busyWin.Start(), 14) // 28 of the 30 the pool takes
	if givenUp(quietWin) || givenUp(olderWin) || givenUp(busyWin) {
		t.Fatal("a request was given up within the budget")
	}
	checkPool(t, p, quiet, older, busy)

	holdOld(busy, busyWin.Start().Add(time.Second), 4) // 32: over
	if !givenUp(olderWin) {
		t.Fatal("the process passed its budget and the oldest request of the apps over their share was not given up")
	}
	if givenUp(quietWin) {
		t.Fatal("the request of an app within its share was given up, though it is the process's oldest")
	}
	if givenUp(busyWin) {
		t.Fatal("the request of the app whose call passed the budget was given up, though another app over its share had an older one")
	}
	if olderWin.Keep() {
		t.Fatal("a kept request given up by the process's budget was recorded")
	}
	checkPool(t, p, quiet, older, busy)
	if got := p.total.Load(); got > p.limit {
		t.Fatalf("after the call the pool counts %d bytes, over its budget %d", got, p.limit)
	}

	// The busy app is now the one over its share (two hold: a share of 15):
	// its own call gives its own request up, under its own lock.
	core, logs := observer.New(zap.DebugLevel)
	busy.SetLogger(zap.New(core))
	holdOld(busy, busyWin.Start().Add(2*time.Second), 14) // 32 of it, 2 of the quiet app's
	if !givenUp(busyWin) || givenUp(quietWin) {
		t.Fatalf("past the budget again: the busy app's request given up %v, the quiet app's %v; want only the busy app's", givenUp(busyWin), givenUp(quietWin))
	}
	var own bool
	for _, e := range logs.FilterMessage("diag/ResolveRange: buffer transition").All() {
		own = own || e.ContextMap()["windows_given_up"] == int64(1)
	}
	if !own {
		t.Fatal("the busy app's own call did not give its own request up")
	}
	checkPool(t, p, quiet, older, busy)
}

// No app is made to give a request up while it holds no more than its share,
// whichever app's call takes the process past its budget: an app holding a
// little for a request older than any other keeps it, and the app holding the
// most gives its own up, when the quiet app's call passes the budget and when
// its own does.
func TestPoolNeverMakesAnAppWithinItsShareGiveUp(t *testing.T) {
	t.Parallel()
	for _, whose := range []string{"the quiet app's call", "the busy app's call"} {
		t.Run(whose, func(t *testing.T) {
			p := newHoldPool(30 * boundUnit) // a share of 15 for each of two apps
			quiet, busy := pooledManager(p), pooledManager(p)
			now := time.Now()
			quietWin := quiet.OpenWindow(now.Add(-120*time.Second), nil)
			busyWin := busy.OpenWindow(now.Add(-60*time.Second), nil)
			defer quietWin.Close()
			defer busyWin.Close()
			holdOld(quiet, quietWin.Start(), 3)
			holdOld(busy, busyWin.Start(), 25)
			if givenUp(quietWin) || givenUp(busyWin) {
				t.Fatal("fixture: a request was given up within the budget")
			}
			if whose == "the quiet app's call" {
				holdOld(quiet, quietWin.Start().Add(time.Second), 3) // 31: over
			} else {
				holdOld(busy, busyWin.Start().Add(time.Second), 3)
			}
			if givenUp(quietWin) {
				t.Fatal("an app within its share gave up its request for another app's hold")
			}
			if !givenUp(busyWin) {
				t.Fatal("past the budget, the app over its share did not give its request up")
			}
			if !quietWin.Keep() {
				t.Fatal("the quiet app's request cannot be recorded")
			}
			checkPool(t, p, quiet, busy)
			if got := p.total.Load(); got > p.limit || got == 0 {
				t.Fatalf("after the call the pool counts %d bytes, want the quiet app's hold, within %d", got, p.limit)
			}
		})
	}
}

// A claimed request is between its Claim and its ResolveKept, which takes what
// it owns: the process's budget passes over its app, gives up the request of
// the next app over its share, and never one of an app within its share; what
// is still over the budget goes with the claimed request's resolve.
func TestPoolPassesOverAClaimedRequest(t *testing.T) {
	t.Parallel()
	p := newHoldPool(30 * boundUnit)
	claimed, over, within := pooledManager(p), pooledManager(p), pooledManager(p)
	now := time.Now()
	claimedWin := claimed.OpenWindow(now.Add(-60*time.Second), nil)
	overWin := over.OpenWindow(now.Add(-50*time.Second), nil)
	withinWin := within.OpenWindow(now.Add(-70*time.Second), nil)
	defer overWin.Close()
	defer withinWin.Close()
	holdOld(within, withinWin.Start(), 2)
	holdOld(over, overWin.Start(), 12)
	if !claimedWin.Claim() {
		t.Fatal("fixture: the request was given up before its claim")
	}
	holdOld(claimed, claimedWin.Start(), 30) // 44 of the 30 the pool takes; a share of 10
	if !givenUp(overWin) {
		t.Fatal("past the process's budget, with the oldest app over its share claimed, the next one's request was not given up")
	}
	if givenUp(claimedWin) || givenUp(withinWin) {
		t.Fatalf("given up: the claimed request %v, the request of the app within its share %v", givenUp(claimedWin), givenUp(withinWin))
	}
	if got := p.total.Load(); got <= p.limit {
		t.Fatalf("fixture: the pool counts %d bytes: the claimed request's hold alone should keep it over %d", got, p.limit)
	}
	claimed.ResolveKept(claimedWin, claimedWin.Start(), time.Now(), "test-claimed", false)
	claimed.mu.Lock()
	held := len(claimed.held)
	claimed.mu.Unlock()
	if held != 0 {
		t.Fatalf("the claimed request's resolve left %d mocks held", held)
	}
	checkPool(t, p, claimed, over, within)
	if got := p.total.Load(); got > p.limit {
		t.Fatalf("after the claimed request's resolve the pool counts %d bytes, over %d", got, p.limit)
	}
}

// holdUnits has the manager's request in flight since start hold n mocks of
// boundUnit each, with the pool's budget out of the way (no other goroutine
// uses p while it does).
func holdUnits(p *holdPool, m *SyncMockManager, start time.Time, n int) {
	limit := p.limit
	p.limit = 1 << 40
	holdOld(m, start, n)
	p.limit = limit
}

// A settle checks again, under the app's lock, what it chose the app for: a
// request claimed since is passed over, and so is an app back within its share
// (another app stopped holding, so the share grew), and the settle then moves
// on to the next app over its share.
func TestPoolChecksItsChoiceUnderTheAppsLock(t *testing.T) {
	t.Parallel()
	for _, since := range []string{"claimed", "back within its share"} {
		t.Run(since, func(t *testing.T) {
			p := newHoldPool(29 * boundUnit)
			chosen, next, leaving := pooledManager(p), pooledManager(p), pooledManager(p)
			now := time.Now()
			chosenWin := chosen.OpenWindow(now.Add(-90*time.Second), nil)
			nextWin := next.OpenWindow(now.Add(-60*time.Second), nil)
			leavingWin := leaving.OpenWindow(now.Add(-30*time.Second), nil)
			defer chosenWin.Close()
			defer nextWin.Close()
			holdUnits(p, chosen, chosenWin.Start(), 14)
			holdUnits(p, next, nextWin.Start(), 16)
			holdUnits(p, leaving, leavingWin.Start(), 3) // 33 of 29: a share of 9 and a bit each
			var chose []*SyncMockManager
			p.chose = func(v *SyncMockManager) {
				chose = append(chose, v)
				if len(chose) > 1 {
					return
				}
				if v != chosen {
					t.Errorf("the settle chose first an app other than the one over its share with the oldest request")
				}
				if since == "claimed" {
					if !chosenWin.Claim() {
						t.Error("fixture: the request was given up before the settle took its app's lock")
					}
					return
				}
				leavingWin.Close() // two apps hold now: a share of 14 and a half, and the chosen app holds 14
			}
			p.settle()
			if givenUp(chosenWin) {
				t.Fatalf("%s since the settle chose its app, the request was given up all the same", since)
			}
			if !givenUp(nextWin) {
				t.Fatalf("the settle stopped at the app it could not give a request up of, though the next app over its share could (chose %d)", len(chose))
			}
			if since == "claimed" {
				chosen.ResolveKept(chosenWin, chosenWin.Start(), time.Now(), "test-chosen", false)
				leavingWin.Close()
			}
			checkPool(t, p, chosen, next, leaving)
			if got := p.total.Load(); got > p.limit {
				t.Fatalf("the pool counts %d bytes, over its budget %d", got, p.limit)
			}
		})
	}
}

// A settle also checks again, under the app's lock, that the pool is still
// over its budget, and that the app's oldest request is still the one it chose
// the app for. Back within the budget (another app's request ended), the
// chosen app keeps its request. With that request ended meanwhile, the settle
// chooses again, and gives up the other app's older request, not the chosen
// app's next, younger one: oldest first holds.
func TestPoolChecksTheBudgetAndTheRequestUnderTheAppsLock(t *testing.T) {
	t.Parallel()
	t.Run("back within the budget", func(t *testing.T) {
		p := newHoldPool(30 * boundUnit)
		v, u, x := pooledManager(p), pooledManager(p), pooledManager(p)
		now := time.Now()
		vw := v.OpenWindow(now.Add(-90*time.Second), nil)
		uw := u.OpenWindow(now.Add(-60*time.Second), nil)
		xw := x.OpenWindow(now.Add(-30*time.Second), nil)
		defer vw.Close()
		defer xw.Close()
		holdUnits(p, v, vw.Start(), 20)
		holdUnits(p, u, uw.Start(), 12)
		holdUnits(p, x, xw.Start(), 1) // 33 of 30: a share of 10
		p.chose = func(c *SyncMockManager) {
			if c == v {
				uw.Close() // 21 of 30, a share of 15: v is still over it
			}
		}
		p.settle()
		if givenUp(vw) {
			t.Fatalf("the pool was back within its budget (%d of %d units) when the settle took the app's lock, and it gave the app's request up all the same",
				p.total.Load()/boundUnit, p.limit/boundUnit)
		}
		checkPool(t, p, v, u, x)
	})
	// The chosen app's oldest request ends between the choice and the lock:
	// the settle chooses again, and where the app's next request is still the
	// oldest of the apps over their share it is the one to go (passing the app
	// would give up the other app's younger request, or, with the other app
	// within its share, leave the pool over its budget with nothing claimed).
	for _, c := range []struct {
		name         string
		other        int  // units the other app holds
		otherGivenUp bool // want
	}{
		{"its next request is the oldest", 16, false},
		{"its next request is the oldest, the other app within its share", 11, false},
	} {
		t.Run("its oldest request ended: "+c.name, func(t *testing.T) {
			p := newHoldPool(30 * boundUnit)
			v, u := pooledManager(p), pooledManager(p)
			now := time.Now()
			w1 := v.OpenWindow(now.Add(-90*time.Second), nil)
			w3 := v.OpenWindow(now.Add(-60*time.Second), nil)
			w2 := u.OpenWindow(now.Add(-30*time.Second), nil)
			defer w2.Close()
			defer w3.Close()
			holdUnits(p, v, w3.Start(), 20) // all of it after w3 started
			holdUnits(p, u, w2.Start(), c.other)
			p.chose = func(m *SyncMockManager) {
				if m == v {
					w1.Close() // v's oldest request ends; v holds 20 still
				}
			}
			p.settle()
			if !givenUp(w3) || givenUp(w2) != c.otherGivenUp {
				t.Fatalf("with the chosen request ended, the settle gave up the chosen app's next request %v and the other app's younger one %v; want only the first",
					givenUp(w3), givenUp(w2))
			}
			checkPool(t, p, v, u)
			if got := p.total.Load(); got > p.limit {
				t.Fatalf("the settle returned with the pool at %d of %d units, nothing claimed", got/boundUnit, p.limit/boundUnit)
			}
		})
	}
	t.Run("its oldest request ended: the other app's request is the oldest now", func(t *testing.T) {
		p := newHoldPool(30 * boundUnit)
		v, u := pooledManager(p), pooledManager(p)
		now := time.Now()
		w1 := v.OpenWindow(now.Add(-90*time.Second), nil)
		w3 := v.OpenWindow(now.Add(-30*time.Second), nil)
		w2 := u.OpenWindow(now.Add(-60*time.Second), nil)
		defer w2.Close()
		defer w3.Close()
		holdUnits(p, v, w3.Start(), 20)
		holdUnits(p, u, w2.Start(), 16) // 36 of 30: a share of 15, both over it
		p.chose = func(m *SyncMockManager) {
			if m == v {
				w1.Close()
			}
		}
		p.settle()
		if givenUp(w3) || !givenUp(w2) {
			t.Fatalf("with the chosen request ended, the settle gave up the chosen app's younger request %v and the other app's older one %v; want only the older",
				givenUp(w3), givenUp(w2))
		}
		checkPool(t, p, v, u)
		if got := p.total.Load(); got > p.limit {
			t.Fatalf("the pool counts %d bytes, over its budget %d", got, p.limit)
		}
	})
}

// A settle ends even where what an app published does not say what it holds
// (its oldest request, here made up): chosen again for the same request, the
// app is passed over, and the next app over its share gives its request up.
// Choosing it for ever would hold every reaper call's settle behind shedMu.
func TestPoolSettleEndsOnAStaleChoice(t *testing.T) {
	t.Parallel()
	p := newHoldPool(30 * boundUnit)
	v, u := pooledManager(p), pooledManager(p)
	now := time.Now()
	vw := v.OpenWindow(now.Add(-60*time.Second), nil)
	uw := u.OpenWindow(now.Add(-30*time.Second), nil)
	defer vw.Close()
	defer uw.Close()
	holdUnits(p, v, vw.Start(), 18)
	holdUnits(p, u, uw.Start(), 16) // 34 of 30: both over a share of 15
	v.oldestUnclaimed.Store(now.Add(-90 * time.Second).UnixNano())
	done := make(chan struct{})
	go func() { defer close(done); p.settle() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the settle chose the app with a stale oldest request for ever")
	}
	if givenUp(vw) || !givenUp(uw) {
		t.Fatalf("given up: the app whose published request is stale %v, the next app over its share %v; want only the next", givenUp(vw), givenUp(uw))
	}
}

// The boundaries are inclusive: an app holding exactly its share is within
// it, and a process holding exactly its budget is within that. An app at its
// share keeps its request, the process's oldest though it is, whether another
// app's call or its own takes the process past its budget, and a settle never
// so much as chooses it (takes its lock); an app chosen while over its share
// and at it by the time the settle takes its lock is passed over. A process
// brought to exactly its budget gives nothing up: not by the call that brought
// it there, not by a settle, and not by a settle that chose an app while the
// process was over it and finds it back at it under the app's lock.
func TestPoolSharesAtTheBoundary(t *testing.T) {
	t.Parallel()
	neverChooses := func(t *testing.T, p *holdPool, within *SyncMockManager) {
		p.chose = func(v *SyncMockManager) {
			if v == within {
				t.Error("a settle chose an app holding exactly its share")
			}
		}
	}
	for _, whose := range []string{"another app's call", "its own call"} {
		t.Run("an app at its share: "+whose, func(t *testing.T) {
			p := newHoldPool(30 * boundUnit) // a share of exactly 10 for each of three apps
			atShare, busy, small := pooledManager(p), pooledManager(p), pooledManager(p)
			now := time.Now()
			atShareWin := atShare.OpenWindow(now.Add(-90*time.Second), nil)
			busyWin := busy.OpenWindow(now.Add(-60*time.Second), nil)
			smallWin := small.OpenWindow(now.Add(-30*time.Second), nil)
			defer atShareWin.Close()
			defer busyWin.Close()
			defer smallWin.Close()
			neverChooses(t, p, atShare)
			if whose == "another app's call" {
				holdOld(atShare, atShareWin.Start(), 10)
				holdOld(busy, busyWin.Start(), 15)
				holdOld(small, smallWin.Start(), 4)
				holdOld(busy, busyWin.Start().Add(time.Second), 2) // 31 of 30
			} else {
				holdUnits(p, atShare, atShareWin.Start(), 9)
				holdUnits(p, busy, busyWin.Start(), 15)
				holdUnits(p, small, smallWin.Start(), 6)                 // 30 of 30
				holdOld(atShare, atShareWin.Start().Add(time.Second), 1) // its share, and 31 of 30
			}
			if givenUp(atShareWin) {
				t.Fatalf("an app holding exactly its share after %s gave up its request, though another app was over its share", whose)
			}
			if !givenUp(busyWin) || givenUp(smallWin) {
				t.Fatalf("past the budget: the app over its share given up %v, the small app's %v; want only the first", givenUp(busyWin), givenUp(smallWin))
			}
			checkPool(t, p, atShare, busy, small)
			if got := p.total.Load(); got > p.limit {
				t.Fatalf("the pool counts %d bytes, over its budget %d", got, p.limit)
			}
		})
	}
	t.Run("chosen, then at its share under its lock", func(t *testing.T) {
		p := newHoldPool(28 * boundUnit) // with two apps, a share of exactly 14
		chosen, next, leaving := pooledManager(p), pooledManager(p), pooledManager(p)
		now := time.Now()
		chosenWin := chosen.OpenWindow(now.Add(-90*time.Second), nil)
		nextWin := next.OpenWindow(now.Add(-60*time.Second), nil)
		leavingWin := leaving.OpenWindow(now.Add(-30*time.Second), nil)
		defer chosenWin.Close()
		defer nextWin.Close()
		holdUnits(p, chosen, chosenWin.Start(), 14)
		holdUnits(p, next, nextWin.Start(), 16)
		holdUnits(p, leaving, leavingWin.Start(), 3) // 33 of 28: a share of 9 and a third each
		var chose []*SyncMockManager
		p.chose = func(v *SyncMockManager) {
			chose = append(chose, v)
			if len(chose) == 1 {
				leavingWin.Close() // two apps hold now: a share of exactly 14, what the chosen app holds
			}
		}
		p.settle()
		if len(chose) == 0 || chose[0] != chosen {
			t.Fatal("fixture: the settle did not choose first the app over its share with the oldest request")
		}
		if givenUp(chosenWin) {
			t.Fatal("the chosen app held exactly its share by the time the settle took its lock, and gave its request up all the same")
		}
		if !givenUp(nextWin) {
			t.Fatal("the settle did not move on to the app still over its share")
		}
		checkPool(t, p, chosen, next, leaving)
		if got := p.total.Load(); got > p.limit {
			t.Fatalf("the pool counts %d bytes, over its budget %d", got, p.limit)
		}
	})
	t.Run("the process at exactly its budget", func(t *testing.T) {
		p := newHoldPool(30 * boundUnit) // with two apps, a share of 15
		big, other := pooledManager(p), pooledManager(p)
		now := time.Now()
		bigWin := big.OpenWindow(now.Add(-90*time.Second), nil)
		otherWin := other.OpenWindow(now.Add(-60*time.Second), nil)
		defer bigWin.Close()
		defer otherWin.Close()
		holdUnits(p, big, bigWin.Start(), 18)
		holdUnits(p, other, otherWin.Start(), 10) // 28 of 30
		chose := 0
		p.chose = func(*SyncMockManager) { chose++ }
		// The big app's call brings the process to exactly its budget: the
		// app is over its share, with the oldest request.
		holdOld(big, bigWin.Start().Add(time.Second), 2)
		if givenUp(bigWin) || givenUp(otherWin) || chose != 0 {
			t.Fatalf("a call that brought the process to exactly its budget gave up the big app's request %v, the other's %v (a settle chose %d)",
				givenUp(bigWin), givenUp(otherWin), chose)
		}
		if got := p.total.Load(); got != p.limit {
			t.Fatalf("fixture: the pool counts %d units, want exactly its budget %d", got/boundUnit, p.limit/boundUnit)
		}
		// A settle with the process at exactly its budget chooses no app.
		p.settle()
		if givenUp(bigWin) || givenUp(otherWin) || chose != 0 {
			t.Fatalf("a settle with the process at exactly its budget gave up the big app's request %v, the other's %v (chose %d)",
				givenUp(bigWin), givenUp(otherWin), chose)
		}
		// Over it when a settle chooses the big app, at exactly it again by
		// the time the settle takes that app's lock (still over its share).
		x := pooledManager(p)
		xWin := x.OpenWindow(now.Add(-30*time.Second), nil)
		holdUnits(p, x, xWin.Start(), 1) // 31 of 30: a share of 10
		p.chose = func(v *SyncMockManager) {
			chose++
			if v != big {
				t.Error("fixture: the settle chose an app other than the big one")
			}
			if chose == 1 {
				xWin.Close() // 30 of 30, a share of 15: the big app's 20 still over it
			}
		}
		p.settle()
		if chose == 0 {
			t.Fatal("fixture: the settle chose no app with the process over its budget")
		}
		if givenUp(bigWin) || givenUp(otherWin) {
			t.Fatalf("the process was at exactly its budget by the time the settle took the app's lock, and it gave up the big app's request %v, the other's %v",
				givenUp(bigWin), givenUp(otherWin))
		}
		checkPool(t, p, big, other, x)
	})
}

// Every reaper call that holds settles the pool before it returns: the stale
// cutoff of a resolve, a duplicate's prune, a synchronous duplicate's window
// and the periodic flush of its late mocks. Here each is a quiet app's call,
// within its share, that takes the process past its budget while another app
// over its share has the oldest request: that request is given up before the
// call returns.
func TestPoolSettlesAfterEachReaperCall(t *testing.T) {
	t.Parallel()
	for _, via := range []string{"the stale cutoff", "a duplicate's prune", "a synchronous duplicate's window", "the periodic flush"} {
		t.Run(via, func(t *testing.T) {
			p := newHoldPool(30 * boundUnit) // a share of 15 for each of two apps
			busy, quiet := pooledManager(p), pooledManager(p)
			now := time.Now()
			busyWin := busy.OpenWindow(now.Add(-90*time.Second), nil)
			quietWin := quiet.OpenWindow(now.Add(-60*time.Second), nil)
			defer busyWin.Close()
			defer quietWin.Close()
			holdOld(busy, busyWin.Start(), 20)
			holdOld(quiet, quietWin.Start(), 4) // 24 of 30
			s := quietWin.Start().Add(10 * time.Second)
			e := s.Add(time.Second)
			add := func() {
				for i := 0; i < 8; i++ { // 32 of 30
					quiet.AddMock(boundMockAt(s.Add(time.Duration(i+1) * time.Millisecond)))
				}
			}
			switch via {
			case "the stale cutoff":
				add()
				k := time.Now()
				quiet.ResolveRange(k, k, "test-k", true, false)
			case "a duplicate's prune":
				add()
				quiet.DeleteMocksStrictlyBefore(time.Now())
			case "a synchronous duplicate's window":
				add()
				quiet.ResolveRange(s, e, "test-dup", false, false)
			case "the periodic flush":
				quiet.ResolveRange(s, e, "test-dup", false, false) // decided before its mocks were decoded
				add()
				quiet.FlushOwnedWindows()
			}
			if got := heldCount(quiet); got != 12 {
				t.Fatalf("fixture: %s held %d of the quiet app's mocks, want 12", via, got)
			}
			if !givenUp(busyWin) || givenUp(quietWin) {
				t.Fatalf("%s took the process past its budget and returned with the busy app's request given up %v, the quiet app's %v",
					via, givenUp(busyWin), givenUp(quietWin))
			}
			checkPool(t, p, busy, quiet)
			if got := p.total.Load(); got > p.limit {
				t.Fatalf("%s returned with the pool at %d bytes, over its budget %d", via, got, p.limit)
			}
		})
	}
}

// A reaper call gives up its own app's request for the pool under its own lock
// only when no settle is running: a running settle decides, and the call
// leaves the excess to it, so the two never each give a request up for the
// same excess. Here the settle is held off until the call has published; the
// call's own settle then gives up the one request.
func TestPoolOwnGiveUpLeavesARunningSettleToDecide(t *testing.T) {
	t.Parallel()
	p := newHoldPool(10 * boundUnit)
	m := pooledManager(p)
	w := m.OpenWindow(time.Now().Add(-60*time.Second), nil)
	defer w.Close()
	holdOld(m, w.Start(), 9)
	p.shedMu.Lock() // a settle running
	done := make(chan struct{})
	go func() {
		defer close(done)
		holdOld(m, w.Start().Add(time.Second), 3) // 12 of 10
	}()
	for deadline := time.Now().Add(10 * time.Second); p.total.Load() <= p.limit && !givenUp(w); {
		if time.Now().After(deadline) {
			p.shedMu.Unlock()
			t.Fatal("the call never published what it held")
		}
		runtime.Gosched()
	}
	if givenUp(w) {
		p.shedMu.Unlock()
		t.Fatal("the call gave its own request up while a settle was running")
	}
	p.shedMu.Unlock()
	<-done
	if !givenUp(w) || m.windowsGivenUp.Load() != 1 {
		t.Fatalf("once the settle let go, the call's settle gave up the request %v (%d given up), want it once", givenUp(w), m.windowsGivenUp.Load())
	}
	if got := p.total.Load(); got > p.limit {
		t.Fatalf("the pool counts %d bytes, over its budget %d", got, p.limit)
	}
}

// A reaper call that gave its own app's request up for the pool publishes
// what that freed before it lets go of shedMu. Here the other app's call has
// published an over-budget hold and its settle waits for shedMu, which the
// own call holds; the own give-up takes the whole excess away. The settle
// that takes shedMu the moment the call lets go of it sees the process within
// its budget and gives up nothing: seeing what was held before the give-up, it
// would give up the other app's request (over its share, and now the oldest of
// those), which the budget no longer requires, and leave its test case out.
func TestPoolOwnGiveUpIsPublishedBeforeTheNextSettle(t *testing.T) {
	t.Parallel()
	p := newHoldPool(30 * boundUnit) // two apps: a share of 15
	m, other := pooledManager(p), pooledManager(p)
	now := time.Now()
	mWin := m.OpenWindow(now.Add(-90*time.Second), nil)
	otherWin := other.OpenWindow(now.Add(-60*time.Second), nil)
	defer mWin.Close()
	defer otherWin.Close()
	holdUnits(p, m, mWin.Start(), 12)
	holdUnits(p, other, otherWin.Start(), 20) // 32 of 30, published; no settle has run
	settled := false
	p.afterOwnGiveUp = func() {
		if settled {
			return
		}
		settled = true
		done := make(chan struct{})
		// Never needs m's lock, which this call holds: m has no request in
		// flight left to give up.
		go func() { defer close(done); p.settle() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the settle did not return")
		}
	}
	holdOld(m, mWin.Start().Add(time.Second), 4) // m: 16, over its share, with the oldest request
	if !settled || !givenUp(mWin) {
		t.Fatalf("fixture: m's call did not give its own request up under its lock (hook ran %v, given up %v)", settled, givenUp(mWin))
	}
	if givenUp(otherWin) {
		t.Fatal("a settle that ran as soon as m's call let go of shedMu gave up the other app's request, though m's own give-up had brought the process within its budget")
	}
	checkPool(t, p, m, other)
	if got := p.total.Load(); got > p.limit {
		t.Fatalf("the pool counts %d bytes, over its budget %d", got, p.limit)
	}
}

// poolDriver drives many managers sharing one pool through what a node agent
// does with them, at random: requests opened, held for (a resolve's stale
// cutoff, a duplicate's prune, a synchronous duplicate's window and the flush
// of its late mocks), kept and resolved, decided duplicates, yielded, flushed.
// Each step is one call, or a call and the flush after it. Every time is drawn
// from a grid relative to base, the driver's start: requests from 40 to 90 s
// before it and their mocks up to 30 s before it, so every mock is past the
// stale cutoff whenever a resolve looks (and only gets older), and a seed
// takes the same path however fast or slow it runs.
type poolDriver struct {
	p    *holdPool
	mgrs []*SyncMockManager
	outs []chan *models.Mock
	wins [][]*Window
	rng  *rand.Rand
	logs *observer.ObservedLogs
	base time.Time
}

func newPoolDriver(p *holdPool, apps int, seed int64) *poolDriver {
	core, logs := observer.New(zap.DebugLevel)
	d := &poolDriver{p: p, mgrs: make([]*SyncMockManager, apps), outs: make([]chan *models.Mock, apps),
		wins: make([][]*Window, apps), rng: rand.New(rand.NewSource(seed)), logs: logs, base: time.Now()}
	for i := range d.mgrs {
		d.mgrs[i], d.outs[i] = pooledManagerOut(p)
		d.mgrs[i].SetLogger(zap.New(core))
	}
	return d
}

// step drives app i once; returns what it did.
func (d *poolDriver) step(i int) string {
	m, rng := d.mgrs[i], d.rng
	pick := func() (int, *Window) {
		if len(d.wins[i]) == 0 {
			return -1, nil
		}
		j := rng.Intn(len(d.wins[i]))
		return j, d.wins[i][j]
	}
	drop := func(j int) { d.wins[i] = append(d.wins[i][:j], d.wins[i][j+1:]...) }
	// at is a time in w's span, at least 30 s before base, on a 1 ms grid.
	at := func(w *Window) time.Time {
		span := d.base.Add(-30 * time.Second).Sub(w.Start())
		return w.Start().Add(time.Duration(rng.Int63n(int64(span/time.Millisecond))) * time.Millisecond)
	}
	mock := func(t time.Time, k int) *models.Mock {
		if rng.Intn(5) == 0 {
			return resultSetMock(t, k, 40+rng.Intn(200), 12) // 50 to 300 KiB
		}
		return boundMockAt(t)
	}
	op := ""
	switch r := rng.Intn(100); {
	case r < 12:
		op = "open"
		if len(d.wins[i]) < 3 {
			d.wins[i] = append(d.wins[i], m.OpenWindow(d.base.Add(-time.Duration(40000+rng.Intn(50000))*time.Millisecond), nil))
		}
	case r < 47:
		op = "hold"
		j, w := pick()
		if j < 0 {
			break
		}
		for k, n := 0, 1+rng.Intn(4); k < n; k++ {
			m.AddMock(mock(at(w), k))
		}
		if rng.Intn(2) == 0 {
			k := time.Now()
			m.ResolveRange(k, k, "test-k", true, false) // the stale cutoff holds what is past it
		} else {
			m.DeleteMocksStrictlyBefore(time.Now()) // a duplicate's prune holds it all
		}
	case r < 60:
		op = "synchronous duplicate"
		j, w := pick()
		if j < 0 {
			break
		}
		s := at(w)
		e := s.Add(500 * time.Millisecond)
		for k, n := 0, 1+rng.Intn(3); k < n; k++ {
			m.AddMock(mock(s.Add(time.Duration(rng.Intn(500))*time.Millisecond), k))
		}
		m.ResolveRange(s, e, "test-dup", false, false) // its window's mocks are held
		for k, n := 0, 1+rng.Intn(2); k < n; k++ {
			m.AddMock(mock(s.Add(time.Duration(rng.Intn(500))*time.Millisecond), k)) // decoded late
		}
		m.FlushOwnedWindows() // the flush holds the late ones
	case r < 73:
		op = "kept"
		if j, w := pick(); j >= 0 {
			if w.Keep() {
				m.ResolveKept(w, w.Start(), time.Now(), "test-kept", false)
			}
			w.Close()
			drop(j)
		}
	case r < 85:
		op = "duplicate"
		if j, w := pick(); j >= 0 {
			w.Close()
			m.DeleteMocksStrictlyBefore(w.Start())
			drop(j)
		}
	case r < 92:
		op = "yield"
		if j, w := pick(); j >= 0 {
			w.Yield(w.Start().Add(time.Duration(rng.Intn(5000)) * time.Millisecond))
		}
	default:
		op = "flush"
		m.FlushOwnedWindows()
	}
	for len(d.outs[i]) > 0 {
		<-d.outs[i]
	}
	return op
}

// closeAll ends every request still in flight.
func (d *poolDriver) closeAll() {
	for i := range d.wins {
		for _, w := range d.wins[i] {
			w.Close()
		}
		d.wins[i] = nil
	}
}

// check fails t unless the process is within its budget, and the pool counts
// what the managers hold.
func (d *poolDriver) check(t *testing.T, what string) {
	t.Helper()
	sum := heldInAll(d.mgrs)
	if got := d.p.total.Load(); got != sum || got > d.p.limit {
		t.Fatalf("%s: the pool counts %d bytes, its managers hold %d, the budget is %d", what, got, sum, d.p.limit)
	}
}

// gaveUpForPool reports how many requests the pool's settle gave up, and fails
// t if any of them was of an app holding no more than its share.
func (d *poolDriver) gaveUpForPool(t *testing.T) int {
	t.Helper()
	lines := d.logs.FilterMessageSnippet("the process's hold passed its budget").All()
	for _, e := range lines {
		f := e.ContextMap()
		if before, share := f["app_held_bytes_before"].(int64), f["app_share_bytes"].(int64); before <= share {
			t.Fatalf("an app holding %d bytes, within its share %d, was made to give a request up", before, share)
		}
	}
	return len(lines)
}

// gaveUpByOwnCall reports how many requests their own manager's reaper calls
// gave up for the budget.
func (d *poolDriver) gaveUpByOwnCall() (n int) {
	for _, e := range d.logs.FilterMessageSnippet("buffer transition").All() {
		if v, ok := e.ContextMap()["windows_given_up"].(int64); ok {
			n += int(v)
		}
	}
	return n
}

// The process total never passes the budget: many apps, each holding for its
// requests in flight, resolving, deciding duplicates, yielding and flushing at
// random, with mocks of 32 KiB and result sets up to 300 KiB, leave the
// process within MaxHeldBytes after every call, and the pool counting exactly
// what they hold. Both ways of giving a request up are taken (an app's own
// call, and another's settle), and the latter never of an app within its
// share.
func TestProcessHoldNeverPassesItsBudget(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= 4; seed++ {
		d := newPoolDriver(newHoldPool(40*boundUnit), 24, seed)
		for s := 0; s < 4000; s++ {
			i := d.rng.Intn(len(d.mgrs))
			op := d.step(i)
			d.check(t, op)
		}
		d.closeAll()
		d.check(t, "every request ended")
		if got := d.p.total.Load(); got != 0 {
			t.Fatalf("seed %d: with no request in flight the pool counts %d bytes", seed, got)
		}
		forPool, own := d.gaveUpForPool(t), d.gaveUpByOwnCall()
		t.Logf("seed %d: requests given up: %d by their own app's call, %d by another's", seed, own, forPool)
		if forPool == 0 || own == 0 {
			t.Fatalf("seed %d: given up by their own app's call %d, by another's %d: the sequences no longer test both", seed, own, forPool)
		}
	}
}

// The same with the apps' calls running at once, as a node agent's do: at
// every point no call is running, the process is within its budget and the
// pool counts what the managers hold; the apps never wedge on each other's
// locks, and the pool is empty once no request is in flight.
func TestProcessHoldStaysWithinItsBudgetUnderConcurrentApps(t *testing.T) {
	t.Parallel()
	const apps, workers, rounds = 12, 6, 3000
	p := newHoldPool(24 * boundUnit)
	drivers := make([]*poolDriver, workers)
	for g := range drivers {
		drivers[g] = newPoolDriver(p, apps/workers, int64(g+1))
	}
	var all []*SyncMockManager
	for _, d := range drivers {
		all = append(all, d.mgrs...)
	}
	// Calls run under quiet's read lock; a check takes its write lock, so it
	// sees the process with no call running.
	var quiet sync.RWMutex
	var wg sync.WaitGroup
	for _, d := range drivers {
		wg.Add(1)
		go func(d *poolDriver) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				quiet.RLock()
				d.step(d.rng.Intn(len(d.mgrs)))
				quiet.RUnlock()
				runtime.Gosched() // let a check in between calls
			}
		}(d)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	checks := 0
	check := func() {
		quiet.Lock()
		defer quiet.Unlock()
		sum := heldInAll(all)
		if got := p.total.Load(); got != sum || got > p.limit {
			t.Errorf("with no call running the pool counts %d bytes, its managers hold %d, the budget is %d", got, sum, p.limit)
		}
		checks++
	}
	deadline := time.Now().Add(120 * time.Second)
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			if time.Now().After(deadline) {
				t.Fatal("the apps wedged")
			}
			check()
			runtime.Gosched()
		}
	}
	check()
	forPool, own := 0, 0
	for _, d := range drivers {
		d.closeAll()
		forPool += d.gaveUpForPool(t)
		own += d.gaveUpByOwnCall()
	}
	t.Logf("%d checks; requests given up: %d by their own app's call, %d by another's", checks, own, forPool)
	if forPool+own == 0 {
		t.Fatal("fixture: the pool never gave a request up")
	}
	checkPool(t, p, all...)
	if got := p.total.Load(); got != 0 {
		t.Fatalf("with no request in flight the pool counts %d bytes", got)
	}
	p.mu.Lock()
	n := len(p.members)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("with no request in flight the pool has %d members", n)
	}
}

// A yield that leaves a held mock to nobody in flight hands it back to its
// owner then (TestALateYieldGivesBackWhatItNoLongerClaims), and the process's
// count lets go of it then too, not at the app's next reaper call: until that,
// it would count against every other app's share of the budget.
func TestPoolLetsGoOfWhatAYieldGivesBack(t *testing.T) {
	t.Parallel()
	p := newHoldPool(1 << 40)
	m := pooledManager(p)
	s0 := time.Now().Add(-30 * time.Second)
	at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }
	a := m.OpenWindow(s0, nil)
	a.Yield(at(100))
	c := m.OpenWindow(at(200), nil)
	defer c.Close()
	m.AddMock(httpMockAt(at(310)))
	a.Keep()
	m.ResolveKept(a, s0, at(400), "test-a", true)
	if heldCount(m) != 1 || p.total.Load() == 0 {
		t.Fatalf("fixture: %d held for the stream that still claims it, %d bytes counted in the pool", heldCount(m), p.total.Load())
	}
	checkPool(t, p, m)
	c.Yield(at(300))
	if heldCount(m) != 0 {
		t.Fatalf("fixture: the yield left %d held", heldCount(m))
	}
	checkPool(t, p, m)
}

// What choosing the request to give up costs past the budget, with 32 apps
// holding (oldestOverShare: once per request given up for the pool).
func BenchmarkPoolChoosesAmong32Apps(b *testing.B) {
	p := newHoldPool(1 << 40)
	now := time.Now()
	for i := 0; i < 32; i++ {
		m := pooledManager(p)
		w := m.OpenWindow(now.Add(-time.Duration(60+i)*time.Second), nil)
		holdOld(m, w.Start(), 1+i%4)
	}
	p.limit = p.total.Load() / 2 // every app over its share
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v, _ := p.oldestOverShare(nil, 0, 0, nil); v == nil {
			b.Fatal("no app over its share")
		}
	}
}

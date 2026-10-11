package manager

import (
	"context"
	"math/rand"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// openWindowManager is a manager past the startup window (so no mock is
// rescued as startup traffic), wired to out and maps.
func openWindowManager(out chan *models.Mock, maps chan models.TestMockMapping) *SyncMockManager {
	return lateMockTestManager(out, maps, false)
}

func sentMocks(out chan *models.Mock) []*models.Mock {
	var got []*models.Mock
	for {
		select {
		case m := <-out:
			got = append(got, m)
		default:
			return got
		}
	}
}

func mappedIDs(maps chan models.TestMockMapping, test string) map[string]bool {
	ids := map[string]bool{}
	for {
		select {
		case e := <-maps:
			if e.TestName == test {
				for _, id := range e.MockIDs {
					ids[id] = true
				}
			}
		default:
			return ids
		}
	}
}

func hasMock(got []*models.Mock, want *models.Mock) bool {
	for _, m := range got {
		if m == want {
			return true
		}
	}
	return false
}

// boundBody is the response body of every boundMockAt mock: one string they
// all share, so a test that fills the hold's budget allocates little, while
// each mock is sized with it (mockSize counts what a mock references).
var boundBody = strings.Repeat("b", 32<<10)

// boundMockAt is a per-test mock requested at reqTS that takes about 32 KiB of
// the hold's budget.
func boundMockAt(reqTS time.Time) *models.Mock {
	mk := httpMockAt(reqTS)
	mk.Spec.HTTPResp = &models.HTTPResp{Body: boundBody}
	return mk
}

// megaBody is a response body a quarter of the hold's budget long, shared like
// boundBody.
var megaBody = strings.Repeat("m", int(MaxHeldBytes/4))

// maxHeldMocks is how many boundMockAt mocks the hold's budget takes: one more
// of them held is over it.
var maxHeldMocks = int(MaxHeldBytes / mockSize(boundMockAt(time.Time{})))

// A request still in flight owns the mocks requested since it started. A
// shorter kept request that resolves once its egress mock is past the 7 s stale
// horizon used to drop that mock (no RESOLVED window owned it), and the slow
// request was then recorded without it, with no warning.
func TestOpenWindowHoldsItsMockPastTheStaleCutoff(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-12 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	early := httpMockAt(start.Add(10 * time.Millisecond)) // its upstream call at t=0
	mgr.AddMock(early)

	k := time.Now().Add(-time.Second)
	kOwn := httpMockAt(k.Add(time.Millisecond))
	mgr.AddMock(kOwn)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true)

	late := httpMockAt(time.Now().Add(-100 * time.Millisecond)) // its call just before it answers
	mgr.AddMock(late)
	if !slow.Keep() {
		t.Fatal("the slow request's window was given up")
	}
	mgr.ResolveKept(slow, start, time.Now(), "test-slow", true)
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("ResolveKept left %d windows open", n)
	}
	slow.Close()

	got := sentMocks(out)
	if !hasMock(got, kOwn) {
		t.Fatalf("the short request's own mock was not recorded")
	}
	if !hasMock(got, early) {
		t.Fatalf("the slow request's early mock was dropped while it was in flight; sent %d mocks", len(got))
	}
	ids := mappedIDs(maps, "test-slow")
	if !ids[early.Name] || !ids[late.Name] {
		t.Fatalf("test-slow's mapping lacks its mocks: early=%v late=%v (%v)", ids[early.Name], ids[late.Name], ids)
	}
	// Recorded in the order they were requested.
	var order []*models.Mock
	for _, m := range got {
		if m == early || m == late {
			order = append(order, m)
		}
	}
	if len(order) != 2 || order[0] != early {
		t.Fatalf("test-slow's mocks out of request order")
	}
}

// A static-dedup duplicate's prune (async capture: every unowned mock before
// its own start) must not take a mock a request still in flight may own.
func TestOpenWindowHoldsItsMockFromADuplicatesPrune(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-3 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	early := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(early)

	dStart := time.Now().Add(-time.Second) // a duplicate that started after it
	dup := mgr.OpenWindow(dStart, nil)
	dup.Close()
	mgr.DeleteMocksStrictlyBefore(dStart)

	slow.Keep()
	mgr.ResolveRange(start, time.Now(), "test-slow", true, true)
	slow.Close()
	if got := sentMocks(out); !hasMock(got, early) {
		t.Fatalf("a duplicate's prune dropped the in-flight request's mock")
	}
	if ids := mappedIDs(maps, "test-slow"); !ids[early.Name] {
		t.Fatalf("test-slow's mapping lacks its mock")
	}
}

// A synchronous duplicate prunes its own window; a mock inside it that a
// request still in flight may own is held for that request. The duplicate
// ends its own window first, so its own window does not hold its debris.
func TestOpenWindowHoldsItsMockFromASynchronousDuplicatesWindow(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-2 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	early := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(early)

	dStart := start.Add(5 * time.Millisecond)
	dup := mgr.OpenWindow(dStart, nil)
	dup.Close() // decided a duplicate: it ends its window before its prune
	mgr.ResolveRange(dStart, start.Add(20*time.Millisecond), "test-0", false, true)

	slow.Keep()
	mgr.ResolveRange(start, time.Now(), "test-slow", true, true)
	slow.Close()
	if got := sentMocks(out); !hasMock(got, early) {
		t.Fatalf("a synchronous duplicate's window prune dropped the in-flight request's mock")
	}
}

// The stale cutoff keeps its purpose: what no window claims once every request
// that may own it is decided is still dropped, and nothing is recorded for it.
// Ending the window drops it at once, for the reason it was held for, and says
// so: it is not put back in the buffer to be rescanned by every later resolve.
func TestOpenWindowLeftoversAreDroppedOnceItEnds(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)
	core, logs := observer.New(zap.DebugLevel)
	mgr.SetLogger(zap.New(core))

	start := time.Now().Add(-12 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	debris := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(debris)
	k := time.Now().Add(-time.Second)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true) // stale: held for slow
	if heldCount(mgr) != 1 {
		t.Fatalf("fixture: held %d, want the debris", heldCount(mgr))
	}

	slow.Close() // decided a duplicate: nothing claims its window's mocks
	mgr.mu.Lock()
	held, buffered := len(mgr.held), len(mgr.buffer)
	mgr.mu.Unlock()
	if held != 0 || buffered != 0 {
		t.Fatalf("ending the window left held=%d buffered=%d, want the leftover dropped", held, buffered)
	}
	var fields map[string]interface{}
	for _, e := range logs.FilterMessageSnippet("window ended").All() {
		fields = e.ContextMap()
	}
	if fields == nil || fields["dropped_outside_any_window"] != int64(1) || fields["dropped"] != int64(1) {
		t.Fatalf("ending the window did not say what it dropped and why: %v", fields)
	}
	n := time.Now()
	mgr.ResolveRange(n, n.Add(5*time.Millisecond), "test-n", true, true)
	if got := sentMocks(out); hasMock(got, debris) {
		t.Fatalf("an unowned leftover was recorded")
	}
}

// A duplicate's own debris is still pruned at once when no other request is
// in flight.
func TestSynchronousDuplicateStillPrunesItsOwnWindow(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	dStart := time.Now().Add(-time.Second)
	dup := mgr.OpenWindow(dStart, nil)
	own := httpMockAt(dStart.Add(time.Millisecond))
	mgr.AddMock(own)
	dup.Close()
	mgr.ResolveRange(dStart, dStart.Add(10*time.Millisecond), "test-0", false, true)
	if got := sentMocks(out); len(got) != 0 {
		t.Fatalf("a duplicate's debris was recorded")
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.buffer) != 0 {
		t.Fatalf("a duplicate's debris was kept: buffer=%d", len(mgr.buffer))
	}
}

// The hold is bounded: past MaxHeldBytes the oldest window not yet kept is
// given up. That request's test case is the whole cost — Keep reports false,
// so it is not recorded — while the mocks only it could own are cleaned up as
// ever, and a younger request in flight keeps everything it owns.
func TestOpenWindowHoldIsBoundedByGivingUpTheOldestWindow(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 64)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)
	var told int
	mgr.OnDrop(func(*models.Mock) { told++ })

	start := time.Now().Add(-20 * time.Second)
	lostTold := 0
	stream := mgr.OpenWindow(start, func(LossCause) func() { lostTold++; return nil }) // in flight for long: a stream, a long poll
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(start.Add(time.Duration(i) * time.Microsecond)))
	}
	youngStart := time.Now().Add(-9 * time.Second)
	young := mgr.OpenWindow(youngStart, nil)
	own := httpMockAt(youngStart.Add(time.Millisecond))
	mgr.AddMock(own)

	k := time.Now().Add(-time.Second)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true)
	if stream.Keep() {
		t.Fatalf("the hold is over its bound and the oldest window was not given up")
	}
	stream.Keep()
	if lostTold != 1 {
		t.Fatalf("the given-up window's lost was told %d times, want once", lostTold)
	}
	if told != 0 {
		t.Fatalf("told %d mocks to OnDrop: a given-up window costs its own test case, not a mock loss for others", told)
	}
	mgr.mu.Lock()
	held := len(mgr.held)
	mgr.mu.Unlock()
	if held > maxHeldMocks {
		t.Fatalf("hold %d over its bound %d", held, maxHeldMocks)
	}
	if !young.Keep() {
		t.Fatalf("a younger window was given up too")
	}
	mgr.ResolveRange(youngStart, time.Now(), "test-young", true, true)
	young.Close()
	if got := sentMocks(out); !hasMock(got, own) || len(got) != 1 {
		t.Fatalf("the younger request's own mock must be recorded, and only it: sent %d", len(got))
	}
	// The given-up request ends as not kept: its window's mocks are pruned,
	// and what it held is cleaned up.
	stream.Close()
	mgr.ResolveRange(start, time.Now(), "test-0", false, true)
	n := time.Now()
	mgr.ResolveRange(n, n.Add(time.Millisecond), "test-n", true, true)
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.held) != 0 || len(mgr.buffer) != 0 {
		t.Fatalf("leftovers of the given-up window remain: held=%d buffer=%d", len(mgr.held), len(mgr.buffer))
	}
}

// A window claimed by Keep is never given up, even over the bound: it is being
// resolved right now and takes what it holds at once.
func TestOpenWindowKeptIsNeverGivenUp(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, maxHeldMocks+16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-20 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(start.Add(time.Duration(i) * time.Microsecond)))
	}
	if !slow.Keep() {
		t.Fatal("fixture: the window was given up before the hold filled")
	}
	k := time.Now().Add(-time.Second)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true)
	if !slow.Keep() {
		t.Fatal("a kept window was given up")
	}
	mgr.ResolveRange(start, time.Now(), "test-slow", true, true)
	slow.Close()
	if got := sentMocks(out); len(got) != maxHeldMocks+5 {
		t.Fatalf("recorded %d of the kept request's %d mocks", len(got), maxHeldMocks+5)
	}
}

// Under memory pressure the hold is let go, and every request that may own a
// mock it drops is given up: a kept one is left out and counted, never
// recorded without its mock. The pressure check on a test case's window does
// not cover this: a request whose response was complete before pressure began,
// with its capture still to run, has a window that ends before the pressure
// span, and its early mock was dropped all the same.
func TestMemoryPressureGivesUpTheRequestsThatMayOwnWhatItDrops(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	core, logs := observer.New(zap.DebugLevel)
	mgr.SetLogger(zap.New(core))
	start := time.Now().Add(-12 * time.Second)
	var causes []LossCause
	slow := mgr.OpenWindow(start, func(c LossCause) func() { causes = append(causes, c); return nil })
	early := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(early)
	k := time.Now().Add(-time.Second)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true) // stale: held for slow
	if heldCount(mgr) != 1 {
		t.Fatalf("fixture: held %d, want the slow request's early mock", heldCount(mgr))
	}
	younger := mgr.OpenWindow(time.Now(), nil) // started after every held mock: owns none of them

	end := time.Now() // the slow request's response is complete; its capture has not run yet
	time.Sleep(time.Millisecond)
	mgr.SetMemoryPressure(true)
	mgr.SetMemoryPressure(false)
	if heldCount(mgr) != 0 {
		t.Fatalf("memory pressure did not let go of the hold: %d held", heldCount(mgr))
	}
	if e := logs.FilterMessageSnippet("memory pressure let go of the mocks held").All(); len(e) != 1 ||
		e[0].ContextMap()["dropped_given_up"] != int64(1) || e[0].ContextMap()["windows_given_up"] != int64(1) {
		t.Fatalf("memory pressure did not say what it let go of the hold: %v", e)
	}
	if on, _ := mgr.WasPressureActiveInWindow(start, end); on {
		t.Fatal("fixture: the slow request's window overlaps the pressure span")
	}
	if slow.Keep() {
		t.Fatal("a request whose early mock memory pressure dropped was kept: it would be recorded without it")
	}
	slow.Close()
	if len(causes) != 1 || causes[0] != LostToMemoryPressure || mgr.KeptRequestsLeftOut() != 1 {
		t.Fatalf("the loss was not told as memory pressure once: causes %v, left out %d", causes, mgr.KeptRequestsLeftOut())
	}
	if !younger.Keep() {
		t.Fatal("a request that owned no dropped mock was given up")
	}
	mgr.ResolveKept(younger, time.Now(), time.Now(), "test-y", true)
	younger.Close()
}

// A window claimed for its resolve keeps what it may own through memory
// pressure: its request is kept already, and ResolveKept is about to take it.
func TestMemoryPressureKeepsWhatAClaimedWindowMayOwn(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-12 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	early := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(early)
	k := time.Now().Add(-time.Second)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true)
	if !slow.Claim() {
		t.Fatal("fixture: the window was given up")
	}
	mgr.SetMemoryPressure(true)
	mgr.SetMemoryPressure(false)
	mgr.ResolveKept(slow, start, time.Now(), "test-slow", true)
	slow.Close()
	if !hasMock(sentMocks(out), early) {
		t.Fatal("memory pressure dropped what a claimed window owned: its kept request is recorded without it")
	}
}

// Held mocks are not pending for any test case: a test case asks once its
// window is resolved, and a kept resolve took every held mock inside it. They
// belong to requests in flight. Counted, one app's slow request would hold back
// the test cases of every app whose window overlaps its held mocks.
func TestPendingInIgnoresHeldMocks(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-12 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	defer slow.Close()
	mgr.AddMock(httpMockAt(start.Add(10 * time.Millisecond)))
	k := time.Now().Add(-time.Second)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true)
	if heldCount(mgr) != 1 {
		t.Fatalf("fixture: held %d", heldCount(mgr))
	}
	if mgr.PendingIn(start, start.Add(time.Second)) {
		t.Fatalf("a mock held for a request in flight holds back the test cases whose window overlaps it")
	}
}

// The resolve's diagnostic says how many mocks THIS call dropped and why:
// duplicate leftovers, and mocks outside any window. Both are cleanup; loss is
// a kept request left out, which Window.Keep warns of. It is not a running
// total.
func TestResolveRangeDiagnosticSplitsDropsByReason(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)
	core, logs := observer.New(zap.DebugLevel)
	mgr.SetLogger(zap.New(core))

	// A synchronous duplicate's window, then a mock of it decoded late, and a
	// mock no window claims, past the stale horizon.
	d := time.Now().Add(-20 * time.Second)
	mgr.ResolveRange(d, d.Add(10*time.Millisecond), "test-0", false, true)
	mgr.AddMock(httpMockAt(d.Add(time.Millisecond)))
	mgr.AddMock(httpMockAt(time.Now().Add(-15 * time.Second)))
	k := time.Now()
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true)

	var fields map[string]interface{}
	for _, e := range logs.FilterMessage("diag/ResolveRange: buffer transition").All() {
		if e.ContextMap()["test_name"] == "test-k" {
			fields = e.ContextMap()
		}
	}
	if fields == nil {
		t.Fatalf("no buffer-transition diagnostic for test-k")
	}
	want := map[string]int64{
		"dropped":                     2,
		"dropped_duplicate_leftovers": 1,
		"dropped_outside_any_window":  1,
		"windows_given_up":            0,
	}
	for k, v := range want {
		got, ok := fields[k].(int64)
		if !ok || got != v {
			t.Fatalf("%s = %v, want %d (fields %v)", k, fields[k], v, fields)
		}
	}
	if _, ok := fields["dropped_total"]; ok {
		t.Fatalf("dropped_total is still logged: it reads as a running total")
	}
}

// OpenWindow with no start registers nothing: a zero start would hold every
// mock.
func TestOpenWindowIgnoresZeroStart(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)
	w := mgr.OpenWindow(time.Time{}, nil)
	if w != nil || !w.Keep() {
		t.Fatalf("a zero start opened a window")
	}
	defer w.Close()
	mgr.AddMock(httpMockAt(time.Now().Add(-12 * time.Second)))
	k := time.Now()
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true)
	if mgr.PendingIn(time.Now().Add(-time.Minute), time.Now()) {
		t.Fatalf("a zero-start window held a mock")
	}
}

// Windows opened and closed while mocks are added and resolved: race-detector
// coverage for the hold.
func TestOpenWindowConcurrentUse(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 4096)
	maps := make(chan models.TestMockMapping, 4096)
	mgr := openWindowManager(out, maps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-out:
			case <-maps:
			case <-ctx.Done():
				return
			}
		}
	}()
	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				s := time.Now().Add(-10 * time.Second)
				w := mgr.OpenWindow(s, nil)
				mgr.AddMock(httpMockAt(s.Add(time.Millisecond)))
				switch i % 3 {
				case 0:
					w.Keep()
					mgr.ResolveRange(s, time.Now(), "test-x", true, true)
				case 1:
					w.Close()
					mgr.DeleteMocksStrictlyBefore(s)
				default:
					mgr.FlushOwnedWindows()
				}
				_ = mgr.PendingIn(s, time.Now())
				w.Close()
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		<-done
	}
}

// Windows end in any order. The hold keeps what a window still open may own and
// lets go of the rest: closing the earliest window lets go only of what was
// requested before the next one's start.
func TestOpenWindowsEndingOutOfOrder(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	t0 := time.Now().Add(-30 * time.Second)
	w1 := mgr.OpenWindow(t0, nil)
	w3 := mgr.OpenWindow(t0.Add(2*time.Second), nil)
	w2 := mgr.OpenWindow(t0.Add(time.Second), nil)
	early := httpMockAt(t0.Add(500 * time.Millisecond))
	late := httpMockAt(t0.Add(1500 * time.Millisecond))
	mgr.AddMock(early)
	mgr.AddMock(late)
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // both stale: held
	if heldCount(mgr) != 2 || mgr.OpenWindows() != 3 {
		t.Fatalf("fixture: held=%d open=%d", heldCount(mgr), mgr.OpenWindows())
	}
	w3.Close() // not the earliest: lets go of nothing
	mgr.mu.Lock()
	held := len(mgr.held)
	mgr.mu.Unlock()
	if held != 2 {
		t.Fatalf("held %d after closing a later window, want 2", held)
	}
	w1.Close() // the earliest: lets go of what only it could own
	mgr.mu.Lock()
	held = len(mgr.held)
	first := mgr.earliestOpenLocked()
	mgr.mu.Unlock()
	if held != 1 || first != w2 {
		t.Fatalf("held %d (want only the mock w2 may own), earliest %p (want w2 %p)", held, first, w2)
	}
	mgr.mu.Lock()
	stillHeld := len(mgr.held) == 1 && mgr.held[0].mock == late
	mgr.mu.Unlock()
	if !stillHeld {
		t.Fatal("the mock w2 may own was let go")
	}
	w2.Keep()
	mgr.ResolveRange(t0.Add(time.Second), time.Now(), "test-w2", true, true)
	w2.Close()
	if got := sentMocks(out); !hasMock(got, late) || hasMock(got, early) {
		t.Fatalf("w2 must record the mock in its window and not the one before it")
	}
}

// Giving a window up is loss only for a request that is then kept: Keep finds
// it given up, its test case is left out, and that is what the manager warns of
// and counts. A given-up window whose request turns out a duplicate costs
// nothing, so giving it up is no warning: the hold's bound alone must not read
// as loss in a loss scan.
func TestOpenWindowGivenUpIsLossOnlyWhenKept(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)
	core, logs := observer.New(zap.DebugLevel)
	mgr.SetLogger(zap.New(core))

	start := time.Now().Add(-30 * time.Second)
	dupLost, keptLost := 0, 0
	dup := mgr.OpenWindow(start, func(LossCause) func() { dupLost++; return nil })
	kept := mgr.OpenWindow(start.Add(time.Second), func(LossCause) func() { keptLost++; return nil })
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(start.Add(2*time.Second + time.Duration(i)*time.Microsecond)))
	}
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // held for both, over the bound: both given up
	if mgr.OpenWindows() != 0 {
		t.Fatalf("fixture: %d windows still open, want both given up", mgr.OpenWindows())
	}
	if w := logs.FilterLevelExact(zap.WarnLevel).All(); len(w) != 0 {
		t.Fatalf("warned at give-up, before any verdict: %q", w[0].Message)
	}
	var diag map[string]interface{}
	for _, e := range logs.FilterMessage("diag/ResolveRange: buffer transition").All() {
		if e.ContextMap()["test_name"] == "test-n" {
			diag = e.ContextMap()
		}
	}
	// What was let go with the given-up windows is the only potential loss:
	// counted on its own, never as cleanup.
	if diag["windows_given_up"] != int64(2) || diag["dropped_given_up"] != int64(maxHeldMocks+5) ||
		diag["dropped_outside_any_window"] != int64(0) || diag["dropped"] != int64(0) {
		t.Fatalf("the diagnostic does not tell what the given-up windows let go from cleanup: %v", diag)
	}

	dup.Close() // decided a duplicate: nothing lost
	if kept.Keep() {
		t.Fatal("a given-up window was kept")
	}
	kept.Keep()
	kept.Close()
	if dupLost != 0 || keptLost != 1 {
		t.Fatalf("lost told: duplicate %d (want 0), kept %d (want 1)", dupLost, keptLost)
	}
	warns := logs.FilterLevelExact(zap.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("%d warnings, want one: the kept request left out", len(warns))
	}
	if got, _ := warns[0].ContextMap()["left_out_so_far"].(uint64); got != 1 {
		t.Fatalf("left_out_so_far = %v, want 1 (fields %v)", warns[0].ContextMap()["left_out_so_far"], warns[0].ContextMap())
	}
	if got := mgr.KeptRequestsLeftOut(); got != 1 {
		t.Fatalf("KeptRequestsLeftOut = %d, want 1", got)
	}
}

// Claim decides like Keep and tells nothing: the loss of a given-up window is
// told by the Keep made afterwards, once, and a claimed window is never given
// up.
func TestOpenWindowClaimTellsNothing(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-30 * time.Second)
	lost := 0
	w := mgr.OpenWindow(start, func(LossCause) func() { lost++; return nil })
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(start.Add(time.Second + time.Duration(i)*time.Microsecond)))
	}
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // over the bound: given up
	if w.Claim() || w.Claim() {
		t.Fatal("a given-up window was claimed")
	}
	if lost != 0 || mgr.KeptRequestsLeftOut() != 0 {
		t.Fatalf("Claim told the loss: lost %d, left out %d", lost, mgr.KeptRequestsLeftOut())
	}
	if w.Keep() {
		t.Fatal("Keep kept a given-up window")
	}
	w.Keep()
	w.Close()
	if lost != 1 || mgr.KeptRequestsLeftOut() != 1 {
		t.Fatalf("the loss was not told once by Keep: lost %d, left out %d", lost, mgr.KeptRequestsLeftOut())
	}

	claimed := mgr.OpenWindow(time.Now().Add(-20*time.Second), nil)
	if !claimed.Claim() {
		t.Fatal("an open window was not claimed")
	}
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(time.Now().Add(-19*time.Second + time.Duration(i)*time.Microsecond)))
	}
	n = time.Now()
	mgr.ResolveRange(n, n, "test-m", true, true)
	if !claimed.Keep() {
		t.Fatal("a claimed window was given up")
	}
	claimed.Close()
}

// A told loss is taken back when a later request is recorded in its request's
// place (a static deduper records one test case per schema, and the schema's
// next request is that one): out of the manager's count, and from whoever the
// window's lost told. Once; and never for a loss that was not told.
func TestWindowReplacedTakesItsLossBack(t *testing.T) {
	t.Parallel()
	mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	start := time.Now().Add(-30 * time.Second)
	onWarning := 0 // what the window's lost keeps: a pod's capture warning
	stream := mgr.OpenWindow(start, func(LossCause) func() {
		onWarning++
		return func() { onWarning-- }
	})
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(start.Add(time.Second + time.Duration(i)*time.Microsecond)))
	}
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // over the budget: given up

	stream.Replaced() // nothing told yet: nothing to take back
	if got := mgr.KeptRequestsLeftOut(); got != 0 || onWarning != 0 {
		t.Fatalf("Replaced before any loss was told: left out %d, on the warning %d", got, onWarning)
	}
	if stream.Keep() {
		t.Fatal("fixture: the window was not given up")
	}
	stream.Close()
	if got := mgr.KeptRequestsLeftOut(); got != 1 || onWarning != 1 {
		t.Fatalf("the loss was not told: left out %d, on the warning %d", got, onWarning)
	}
	stream.Replaced()
	stream.Replaced()
	if got := mgr.KeptRequestsLeftOut(); got != 0 || onWarning != 0 {
		t.Fatalf("the loss was not taken back once: left out %d, on the warning %d", got, onWarning)
	}
	stream.Keep() // told once only: a later Keep tells nothing again
	if got := mgr.KeptRequestsLeftOut(); got != 0 || onWarning != 0 {
		t.Fatalf("a loss taken back was told again: left out %d, on the warning %d", got, onWarning)
	}

	kept := mgr.OpenWindow(time.Now(), nil)
	if !kept.Keep() {
		t.Fatal("fixture: an open window was not kept")
	}
	kept.Replaced()
	kept.Close()
	var none *Window
	none.Replaced()
	if got := mgr.KeptRequestsLeftOut(); got != 0 {
		t.Fatalf("Replaced on a window that lost nothing changed the count to %d", got)
	}
}

// lostConn stands for what a window's lost closes over: the request's
// connection, with its buffers.
type lostConn struct{ buf [1 << 16]byte }

// A window whose loss was told stays referenced for as long as the loss
// stands: whoever may take it back holds its Replaced, which can be for the
// rest of the recording (a schema left out and never recorded since). lost
// closes over the request's connection, so the window lets go of it once it
// has told it.
func TestWindowLetsGoOfLostOnceItHasToldIt(t *testing.T) {
	mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	start := time.Now().Add(-30 * time.Second)
	freed := make(chan struct{})
	open := func() *Window {
		conn := new(lostConn)
		runtime.SetFinalizer(conn, func(*lostConn) { close(freed) })
		return mgr.OpenWindow(start, func(LossCause) func() {
			conn.buf[0]++
			return func() {}
		})
	}
	stream := open()
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(start.Add(time.Second + time.Duration(i)*time.Microsecond)))
	}
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // over the budget: given up
	if stream.Keep() {
		t.Fatal("fixture: the window was not given up")
	}
	stream.Close()
	takeBack := stream.Replaced // what a deduper keeps while the loss stands

	deadline := time.After(10 * time.Second)
	for {
		runtime.GC()
		select {
		case <-freed:
			takeBack()
			if got := mgr.KeptRequestsLeftOut(); got != 0 {
				t.Fatalf("the loss was not taken back: left out %d", got)
			}
			return
		case <-deadline:
			t.Fatal("a window whose loss was told still holds what its lost closed over")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func heldCount(mgr *SyncMockManager) int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return len(mgr.held)
}

// ResolveKept ends the request's window in the critical section that takes
// what was held for it, before it hands the mocks on. A send can wait on a full
// output channel (sendBudget per mock); a window left open meanwhile pinned the
// hold, and the bound — which never gives up a claimed window — let it grow
// without limit. OnDrop runs inside those sends, after the lock is let go, so
// the test looks from there.
func TestResolveKeptEndsTheWindowBeforeItsSends(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock) // never read: every send waits out its budget
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	start := time.Now().Add(-20 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	mgr.AddMock(httpMockAt(start.Add(time.Millisecond)))
	if !slow.Claim() {
		t.Fatal("fixture: given up")
	}
	end := time.Now()
	var openDuringSend, heldDuringSend int
	var looked atomic.Bool
	mgr.OnDrop(func(*models.Mock) {
		if !looked.CompareAndSwap(false, true) {
			return
		}
		openDuringSend = mgr.OpenWindows()
		// Droppable traffic arrives while the resolve sends: a flood of a
		// duplicate's debris after the slow request's response, so outside
		// its resolved window, and after its start, so held while it is open.
		for i := 0; i < maxHeldMocks+5; i++ {
			mgr.AddMock(boundMockAt(end.Add(time.Millisecond + time.Duration(i)*time.Microsecond)))
		}
		mgr.DeleteMocksStrictlyBefore(time.Now())
		heldDuringSend = heldCount(mgr)
	})
	mgr.ResolveKept(slow, start, end, "test-slow", true)
	slow.Close()
	if !looked.Load() {
		t.Fatal("fixture: the resolve's send never waited out its budget")
	}
	if openDuringSend != 0 {
		t.Fatalf("the window was still open while its resolve sent its mocks (%d open)", openDuringSend)
	}
	if heldDuringSend > maxHeldMocks {
		t.Fatalf("the hold grew to %d past its bound %d while a resolve sent", heldDuringSend, maxHeldMocks)
	}
}

// With the output not wired yet, a kept resolve leaves its in-window mocks in
// the buffer for later; the held ones go there too rather than being dropped
// when its window ends, and the first resolve with the output wired records
// them for it.
func TestResolveKeptBeforeTheOutputIsWiredKeepsItsHeldMocks(t *testing.T) {
	t.Parallel()
	maps := make(chan models.TestMockMapping, 16)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
	mgr.SetMappingChannel(context.Background(), maps)
	mgr.SetFirstRequestSignaled()
	mgr.resolvedTestCount = models.StartupMockTestCaseWindow

	start := time.Now().Add(-12 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	early := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(early)
	k := time.Now().Add(-time.Second)
	mgr.ResolveRange(k, k.Add(5*time.Millisecond), "test-k", true, true) // stale: held for slow
	if heldCount(mgr) != 1 {
		t.Fatalf("fixture: held %d", heldCount(mgr))
	}
	slow.Keep()
	mgr.ResolveKept(slow, start, time.Now(), "test-slow", true)
	slow.Close()

	out := make(chan *models.Mock, 16)
	mgr.SetOutputChannel(out)
	mgr.FlushOwnedWindows()
	if !hasMock(sentMocks(out), early) {
		t.Fatal("a kept request's held mock was dropped because the output was not wired at its resolve")
	}
}

// A static-dedup duplicate's late mock — decoded after its window resolved,
// inside it — is its debris, but a request still in flight may own it: the
// periodic flush and a later resolve's retroactive bin hold it for that
// request rather than drop it.
func TestDuplicatesLateMockIsHeldForAnOpenWindow(t *testing.T) {
	t.Parallel()
	for _, via := range []string{"FlushOwnedWindows", "ResolveRange"} {
		t.Run(via, func(t *testing.T) {
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := openWindowManager(out, maps)

			start := time.Now().Add(-3 * time.Second)
			slow := mgr.OpenWindow(start, nil)
			d := start.Add(100 * time.Millisecond)
			mgr.ResolveRange(d, d.Add(10*time.Millisecond), "test-0", false, true) // a sync duplicate inside slow's window
			late := httpMockAt(d.Add(time.Millisecond))                            // decoded after the duplicate resolved
			mgr.AddMock(late)
			switch via {
			case "FlushOwnedWindows":
				mgr.FlushOwnedWindows()
			default:
				k := time.Now()
				mgr.ResolveRange(k, k.Add(time.Millisecond), "test-k", true, true)
			}
			if heldCount(mgr) != 1 {
				t.Fatalf("%s dropped a duplicate's late mock a request in flight may own (held %d)", via, heldCount(mgr))
			}
			slow.Keep()
			mgr.ResolveKept(slow, start, time.Now(), "test-slow", true)
			slow.Close()
			if !hasMock(sentMocks(out), late) {
				t.Fatalf("%s: the request in flight was recorded without the mock held for it", via)
			}
		})
	}
}

// The bound holds on every reaper that holds: a duplicate's prune and the
// periodic flush give up the oldest window once the hold passes MaxHeldBytes,
// as a resolve does.
func TestPruneAndFlushEnforceTheHoldBound(t *testing.T) {
	t.Parallel()
	for _, via := range []string{"DeleteMocksStrictlyBefore", "FlushOwnedWindows"} {
		t.Run(via, func(t *testing.T) {
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := openWindowManager(out, maps)

			start := time.Now().Add(-30 * time.Second)
			stream := mgr.OpenWindow(start, nil)
			d := start.Add(time.Second)
			if via == "FlushOwnedWindows" {
				mgr.ResolveRange(d, d.Add(time.Second), "test-0", false, true) // a duplicate's window the flood lands in
			}
			for i := 0; i < maxHeldMocks+5; i++ {
				mgr.AddMock(boundMockAt(d.Add(time.Duration(i) * time.Microsecond)))
			}
			if via == "FlushOwnedWindows" {
				mgr.FlushOwnedWindows()
			} else {
				mgr.DeleteMocksStrictlyBefore(time.Now())
			}
			if n := heldCount(mgr); n > maxHeldMocks {
				t.Fatalf("%s left %d held, past the bound %d", via, n, maxHeldMocks)
			}
			if stream.Keep() {
				t.Fatalf("%s did not give up the oldest window past the bound", via)
			}
			stream.Close()
		})
	}
}

// The prune's and the flush's diagnostics say what THIS call dropped and why,
// as the resolve's does: their drops were silent.
func TestPruneAndFlushDiagnosticsSplitDropsByReason(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)
	core, logs := observer.New(zap.DebugLevel)
	mgr.SetLogger(zap.New(core))

	d := time.Now().Add(-3 * time.Second)
	mgr.ResolveRange(d, d.Add(10*time.Millisecond), "test-0", false, true) // a sync duplicate
	mgr.AddMock(httpMockAt(d.Add(time.Millisecond)))                       // its late debris
	mgr.FlushOwnedWindows()
	mgr.AddMock(httpMockAt(time.Now().Add(-2 * time.Second))) // an async duplicate's debris
	slow := mgr.OpenWindow(time.Now().Add(-time.Second), nil)
	mgr.AddMock(httpMockAt(time.Now().Add(-500 * time.Millisecond))) // may be slow's
	mgr.DeleteMocksStrictlyBefore(time.Now())
	slow.Close()

	for msg, want := range map[string]map[string]int64{
		"diag/FlushOwnedWindows: buffer transition":         {"dropped": 1, "dropped_duplicate_leftovers": 1, "dropped_outside_any_window": 0, "held_now": 0},
		"diag/DeleteMocksStrictlyBefore: buffer transition": {"dropped": 1, "dropped_duplicate_leftovers": 1, "dropped_outside_any_window": 0, "held_now": 1},
	} {
		entries := logs.FilterMessage(msg).All()
		if len(entries) != 1 {
			t.Fatalf("%d %q lines, want 1", len(entries), msg)
		}
		f := entries[0].ContextMap()
		for k, v := range want {
			if got, ok := f[k].(int64); !ok || got != v {
				t.Fatalf("%s: %s = %v, want %d (fields %v)", msg, k, f[k], v, f)
			}
		}
	}
}

// Randomised sequences of open / claim / close / resolve / prune / flush /
// pressure against a model: checks the heap's index bookkeeping, the hold's
// order and its floor, that no mock is both sent and still held/buffered, and
// that no mock is sent twice.
func TestOpenWindowRandomInvariants(t *testing.T) {
	t.Parallel()
	byBudget := 0   // windows the hold's budget gave up, over all seeds
	sharedSeen := 0 // checks that found a held mock with an owner (heldMock.owner)
	for seed := int64(1); seed <= 150; seed++ {
		rng := rand.New(rand.NewSource(seed))
		out := make(chan *models.Mock, 1<<16)
		maps := make(chan models.TestMockMapping, 1<<16)
		mgr := openWindowManager(out, maps)
		base := time.Now().Add(-60 * time.Second)
		at := func() time.Time { return base.Add(time.Duration(rng.Intn(50_000)) * time.Millisecond) }
		type win struct {
			w        *Window
			start    time.Time
			closed   bool
			claimed  bool
			resolved bool
		}
		var wins []*win
		sent := map[*models.Mock]int{}
		all := map[*models.Mock]bool{}
		drain := func() {
			for {
				select {
				case m := <-out:
					sent[m]++
				default:
					return
				}
			}
		}
		check := func(step int, op string) {
			drain()
			mgr.mu.Lock()
			defer mgr.mu.Unlock()
			for i, w := range mgr.open {
				if w.idx != i {
					t.Fatalf("seed %d step %d (%s): open[%d].idx = %d", seed, step, op, i, w.idx)
				}
				if w.givenUp {
					t.Fatalf("seed %d step %d (%s): a given-up window is still in the heap", seed, step, op)
				}
				if i > 0 {
					p := (i - 1) / 2
					if mgr.open[i].start.Before(mgr.open[p].start) {
						t.Fatalf("seed %d step %d (%s): heap order broken at %d", seed, step, op, i)
					}
				}
			}
			inHeap := map[*Window]bool{}
			for _, w := range mgr.open {
				inHeap[w] = true
			}
			for _, x := range wins {
				if !inHeap[x.w] && x.w.idx != -1 {
					t.Fatalf("seed %d step %d (%s): window out of the heap has idx %d", seed, step, op, x.w.idx)
				}
				if x.closed && inHeap[x.w] {
					t.Fatalf("seed %d step %d (%s): a closed window is still open", seed, step, op)
				}
			}
			var floor time.Time
			if len(mgr.open) > 0 {
				floor = mgr.open[0].start
			}
			if len(mgr.open) == 0 && len(mgr.held) != 0 {
				t.Fatalf("seed %d step %d (%s): %d held with no window open", seed, step, op, len(mgr.held))
			}
			for i, h := range mgr.held {
				if i > 0 && h.mock.Spec.ReqTimestampMock.Before(mgr.held[i-1].mock.Spec.ReqTimestampMock) {
					t.Fatalf("seed %d step %d (%s): hold out of order at %d", seed, step, op, i)
				}
				if h.mock.Spec.ReqTimestampMock.Before(floor) {
					t.Fatalf("seed %d step %d (%s): held mock before the earliest open start", seed, step, op)
				}
				if sent[h.mock] > 0 {
					t.Fatalf("seed %d step %d (%s): a held mock was also sent", seed, step, op)
				}
			}
			seen := map[*models.Mock]bool{}
			for _, h := range mgr.held {
				if seen[h.mock] {
					t.Fatalf("seed %d step %d (%s): mock held twice", seed, step, op)
				}
				seen[h.mock] = true
			}
			for _, b := range mgr.buffer {
				if seen[b] {
					t.Fatalf("seed %d step %d (%s): mock both buffered and held", seed, step, op)
				}
				if sent[b] > 0 {
					t.Fatalf("seed %d step %d (%s): a buffered mock was also sent", seed, step, op)
				}
				seen[b] = true
			}
			for _, o := range mgr.owed {
				if seen[o.mock] {
					t.Fatalf("seed %d step %d (%s): an owed mock is also buffered or held", seed, step, op)
				}
				if sent[o.mock] > 0 {
					t.Fatalf("seed %d step %d (%s): an owed mock was also sent", seed, step, op)
				}
				if o.to == nil || !o.to.keep {
					t.Fatalf("seed %d step %d (%s): a mock owed to no kept window", seed, step, op)
				}
				seen[o.mock] = true
			}
			for m, n := range sent {
				if n > 1 {
					t.Fatalf("seed %d step %d (%s): mock sent %d times", seed, step, op, n)
				}
				_ = m
			}
			// The budget bounds all of it, whatever its age.
			if mgr.heldBytes > MaxHeldBytes && !(len(mgr.open) > 0 && mgr.open[0].kept) {
				t.Fatalf("seed %d step %d (%s): %d bytes held, over the budget, with an unclaimed earliest window", seed, step, op, mgr.heldBytes)
			}
			// The hold's counts are the hold's, and the pool's are its.
			var all int64
			shared := 0
			for _, h := range mgr.held {
				all += h.size
				if h.owner != nil {
					shared++
				}
			}
			if pool := mgr.poolLocked(); mgr.pooled != mgr.heldBytes || pool.total.Load() != mgr.heldBytes {
				t.Fatalf("seed %d step %d (%s): the hold counts %d bytes, published %d, its pool counts %d", seed, step, op, mgr.heldBytes, mgr.pooled, pool.total.Load())
			}
			if shared != mgr.heldShared {
				t.Fatalf("seed %d step %d (%s): the hold counts %d shared mocks; it holds %d", seed, step, op, mgr.heldShared, shared)
			}
			if shared > 0 {
				sharedSeen++
			}
			if all != mgr.heldBytes {
				t.Fatalf("seed %d step %d (%s): the hold counts %d bytes; it holds %d", seed, step, op, mgr.heldBytes, all)
			}
		}
		for step := 0; step < 400; step++ {
			op := ""
			switch r := rng.Intn(100); {
			case r < 22:
				op = "open"
				s := at()
				wins = append(wins, &win{w: mgr.OpenWindow(s, nil), start: s})
			case r < 50:
				op = "add"
				n := 1 + rng.Intn(6)
				for i := 0; i < n; i++ {
					// A mock of any size: a quarter of them take a quarter of
					// the budget each, so the sequences meet the bound.
					m := httpMockAt(at())
					if rng.Intn(4) == 0 {
						m.Spec.HTTPResp = &models.HTTPResp{Body: megaBody}
					}
					all[m] = true
					mgr.AddMock(m)
				}
			case r < 60:
				op = "resolve-kept-window"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed || x.resolved {
					continue
				}
				if x.w.Claim() {
					x.claimed = true
					if rng.Intn(4) == 0 { // pressure or a reaper between claim and resolve
						mgr.SetMemoryPressure(true)
						mgr.SetMemoryPressure(false)
					}
					mgr.ResolveKept(x.w, x.start, x.start.Add(time.Duration(rng.Intn(5000))*time.Millisecond), "t", true)
					x.resolved = true
				}
				x.w.Close()
				x.closed = true
			case r < 70:
				op = "dup-sync"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed {
					continue
				}
				x.w.Close()
				x.closed = true
				mgr.ResolveRange(x.start, x.start.Add(time.Duration(rng.Intn(5000))*time.Millisecond), "test-0", false, true)
			case r < 80:
				op = "dup-async"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed {
					continue
				}
				x.w.Close()
				x.closed = true
				mgr.DeleteMocksStrictlyBefore(x.start)
			case r < 86:
				op = "flush"
				mgr.FlushOwnedWindows()
			case r < 90:
				op = "pressure"
				mgr.SetMemoryPressure(true)
				mgr.SetMemoryPressure(false)
			case r < 95:
				op = "unwindowed-kept"
				s := at()
				mgr.ResolveRange(s, s.Add(time.Duration(rng.Intn(3000))*time.Millisecond), "u", true, true)
			case r < 98:
				// The synchronous ingress giving its lock back: the window's
				// kept resolve then leaves what another request claims.
				op = "yield"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed {
					continue
				}
				y := x.start.Add(time.Duration(rng.Intn(3000)) * time.Millisecond)
				x.w.Yield(y)
				// The next request, which runs beside the rest of it.
				s := y.Add(time.Duration(rng.Intn(500)) * time.Millisecond)
				wins = append(wins, &win{w: mgr.OpenWindow(s, nil), start: s})
				for i, n := 0, 1+rng.Intn(3); i < n; i++ {
					m := httpMockAt(s.Add(time.Duration(rng.Intn(1000)) * time.Millisecond))
					all[m] = true
					mgr.AddMock(m)
				}
			default:
				op = "close"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				x.w.Close()
				x.w.Close()
				x.closed = true
			}
			check(step, op)
		}
		for _, x := range wins {
			x.w.Close()
		}
		check(-1, "final")
		if n := mgr.OpenWindows(); n != 0 {
			t.Fatalf("seed %d: %d windows open after all closed", seed, n)
		}
		byBudget += givenUpByBudget(mgr, func(yield func(*Window)) {
			for _, x := range wins {
				yield(x.w)
			}
		})
	}
	t.Logf("the hold's budget gave up %d windows over the seeds; %d checks found shared mocks held", byBudget, sharedSeen)
	if byBudget == 0 {
		t.Fatal("no seed took the hold past its budget: the sequences no longer test the bound")
	}
	if sharedSeen == 0 {
		t.Fatal("no check found a shared mock held: the sequences no longer test what a yielded window leaves")
	}
}

// givenUpByBudget counts the windows among those each yields that the hold's
// budget gave up.
func givenUpByBudget(mgr *SyncMockManager, each func(yield func(*Window))) (n int) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	each(func(w *Window) {
		if w.givenUp && w.cause == LostToHoldBound {
			n++
		}
	})
	return n
}

// The bound, with many windows and a flood arriving in random request order:
// the hold is kept sorted on insertion, and the oldest windows are given up
// until it fits.
func TestOpenWindowBoundWithRandomArrival(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	out := make(chan *models.Mock, 16)
	mgr := openWindowManager(out, make(chan models.TestMockMapping, 16))
	base := time.Now().Add(-120 * time.Second)
	var ws []*Window
	for i := 0; i < 50; i++ {
		ws = append(ws, mgr.OpenWindow(base.Add(time.Duration(i)*time.Second), nil))
	}
	n := 3 * maxHeldMocks
	perm := rng.Perm(n)
	for _, i := range perm {
		// Spread over the 50 s the windows open across.
		mgr.AddMock(boundMockAt(base.Add(time.Duration(i) * 49 * time.Second / time.Duration(n))))
	}
	k := time.Now()
	mgr.ResolveRange(k, k, "k", true, false)
	given := 0
	for _, w := range ws {
		if !w.Claim() {
			given++
		}
	}
	if heldCount(mgr) > maxHeldMocks || given == 0 {
		t.Fatalf("hold %d over its bound %d, %d windows given up", heldCount(mgr), maxHeldMocks, given)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	for i := 1; i < len(mgr.held); i++ {
		if mgr.held[i].mock.Spec.ReqTimestampMock.Before(mgr.held[i-1].mock.Spec.ReqTimestampMock) {
			t.Fatalf("hold out of request order at %d", i)
		}
	}
}

// A kept window takes a held mock requested exactly at its end: the window is
// [start, end], both ends in, as for buffered mocks.
func TestKeptResolveTakesAHeldMockAtItsEnd(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	mgr := openWindowManager(out, make(chan models.TestMockMapping, 16))
	start := time.Now().Add(-20 * time.Second)
	w := mgr.OpenWindow(start, nil)
	end := start.Add(time.Second)
	atEnd := httpMockAt(end)
	mgr.AddMock(atEnd)
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // stale: held
	if heldCount(mgr) != 1 {
		t.Fatalf("fixture: held %d", heldCount(mgr))
	}
	w.Keep()
	mgr.ResolveKept(w, start, end, "test-w", true)
	w.Close()
	if !hasMock(sentMocks(out), atEnd) {
		t.Fatal("a held mock requested at the window's end was not recorded with it")
	}
}

// Ending the earliest window keeps what the next one may own from its start
// on, a mock requested exactly at that start included.
func TestEndingAWindowKeepsWhatTheNextMayOwnFromItsStart(t *testing.T) {
	t.Parallel()
	mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	t0 := time.Now().Add(-20 * time.Second)
	first := mgr.OpenWindow(t0, nil)
	next := mgr.OpenWindow(t0.Add(time.Second), nil)
	before := httpMockAt(t0.Add(500 * time.Millisecond))
	atNext := httpMockAt(t0.Add(time.Second))
	mgr.AddMock(before)
	mgr.AddMock(atNext)
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // both stale: held
	first.Close()
	mgr.mu.Lock()
	kept := len(mgr.held) == 1 && mgr.held[0].mock == atNext
	mgr.mu.Unlock()
	if !kept {
		t.Fatalf("ending the earliest window dropped a mock requested at the next window's start (held %d)", heldCount(mgr))
	}
	next.Close()
}

// A duplicate's resolve takes nothing held: what is held is for a request still
// in flight, whose own verdict decides it.
func TestDuplicateResolveTakesNothingHeld(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	mgr := openWindowManager(out, make(chan models.TestMockMapping, 16))
	start := time.Now().Add(-20 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	mine := httpMockAt(start.Add(time.Second))
	mgr.AddMock(mine)
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true) // stale: held for slow
	mgr.ResolveRange(start, time.Now(), "test-0", false, true)
	if heldCount(mgr) != 1 || len(sentMocks(out)) != 0 {
		t.Fatalf("a duplicate's resolve took a mock held for a request in flight (held %d)", heldCount(mgr))
	}
	slow.Keep()
	mgr.ResolveKept(slow, start, time.Now(), "test-slow", true)
	slow.Close()
	if !hasMock(sentMocks(out), mine) {
		t.Fatal("the request in flight was not recorded with its held mock")
	}
}

// A randomised model of the SAFETY property the windows are for (the invariant
// test above checks structure: heap indices, hold order, no double send):
//
//	S1: no operation makes a mock vanish (neither sent, buffered nor held)
//	    while a window that may own it (start <= mock time) is still open.
//	S2: a kept resolve of a claimed window sends every mock in its range that
//	    was buffered or held when it ran.
//	S3: a window that is claimed (never given up) is resolved with every mock
//	    added while it was open inside [start, end] sent by then.
//	S4: the hold is within its bound after every operation.
func TestOpenWindowNoOpenWindowLosesAMock(t *testing.T) {
	t.Parallel()
	seeds := int64(40) // ~8 s under -race
	if testing.Short() {
		seeds = 10
	}
	byBudget := 0       // windows the hold's budget gave up, over all seeds
	leftAfterYield := 0 // mocks a yielded window's resolve left to another request
	for seed := int64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewSource(seed))
		out := make(chan *models.Mock, 1<<17)
		maps := make(chan models.TestMockMapping, 1<<17)
		mgr := openWindowManager(out, maps)
		base := time.Now().Add(-70 * time.Second)
		at := func() time.Time { return base.Add(time.Duration(rng.Intn(68_000)) * time.Millisecond) }

		type win struct {
			w       *Window
			start   time.Time
			closed  bool
			during  []*models.Mock // added while it was open, at or after its start
			givenUp bool
			yield   time.Time // its earliest Yield, if any
		}
		var wins []*win
		sent := map[*models.Mock]bool{}
		leftBehind := map[*models.Mock]bool{} // left by a yielded window's resolve (S2, S3)
		drain := func() {
			for {
				select {
				case m := <-out:
					if sent[m] {
						t.Fatalf("seed %d: mock sent twice", seed)
					}
					sent[m] = true
				case <-maps:
				default:
					return
				}
			}
		}
		present := func() map[*models.Mock]bool {
			mgr.mu.Lock()
			defer mgr.mu.Unlock()
			p := make(map[*models.Mock]bool, len(mgr.buffer)+len(mgr.held))
			for _, b := range mgr.buffer {
				p[b] = true
			}
			for _, h := range mgr.held {
				p[h.mock] = true
			}
			return p
		}
		add := func(m *models.Mock) {
			mgr.AddMock(m)
			for _, x := range wins {
				if !x.closed && !m.Spec.ReqTimestampMock.Before(x.start) {
					x.during = append(x.during, m)
				}
			}
		}
		for step := 0; step < 300; step++ {
			before := present()
			op := ""
			var resolved *win
			var rStart, rEnd time.Time
			switch r := rng.Intn(100); {
			case r < 20:
				op = "open"
				s := at()
				wins = append(wins, &win{w: mgr.OpenWindow(s, nil), start: s})
			case r < 45:
				op = "add"
				for i, n := 0, 1+rng.Intn(6); i < n; i++ {
					add(httpMockAt(at()))
				}
			case r < 47:
				op = "flood"
				s := at()
				// Twice the hold's budget, in two seconds of request time.
				for i := 0; i < 2*maxHeldMocks; i++ {
					add(boundMockAt(s.Add(time.Duration(rng.Intn(2_000_000)) * time.Microsecond)))
				}
			case r < 60:
				op = "resolve-kept-window"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed {
					continue
				}
				if x.w.Claim() {
					rStart, rEnd = x.start, x.start.Add(time.Duration(rng.Intn(8000))*time.Millisecond)
					mgr.ResolveKept(x.w, rStart, rEnd, "t", true)
					resolved = x
				} else {
					x.givenUp = true
					x.w.Close()
					mgr.ResolveRange(x.start, x.start.Add(time.Second), "", false, false)
				}
				x.w.Close()
				x.closed = true
			case r < 70:
				op = "dup-sync"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed {
					continue
				}
				x.w.Close()
				x.closed = true
				mgr.ResolveRange(x.start, x.start.Add(time.Duration(rng.Intn(5000))*time.Millisecond), "test-0", false, true)
			case r < 80:
				op = "dup-async"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed {
					continue
				}
				x.w.Close()
				x.closed = true
				mgr.DeleteMocksStrictlyBefore(x.start)
			case r < 86:
				op = "flush"
				mgr.FlushOwnedWindows()
			case r < 89:
				op = "pressure"
				mgr.SetMemoryPressure(true)
				mgr.SetMemoryPressure(false)
			case r < 95:
				op = "unwindowed-kept"
				s := at()
				mgr.ResolveRange(s, s.Add(time.Duration(rng.Intn(3000))*time.Millisecond), "u", true, true)
			case r < 98:
				// The synchronous ingress giving its lock back: the window's
				// kept resolve then leaves what another request claims after
				// it (S2, S3), which must not vanish meanwhile (S1).
				op = "yield"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				if x.closed {
					continue
				}
				y := x.start.Add(time.Duration(rng.Intn(8000)) * time.Millisecond)
				x.w.Yield(y)
				if x.yield.IsZero() || y.Before(x.yield) {
					x.yield = y
				}
				// The next request, which runs beside the rest of it.
				s := y.Add(time.Duration(rng.Intn(500)) * time.Millisecond)
				wins = append(wins, &win{w: mgr.OpenWindow(s, nil), start: s})
				for i, n := 0, 1+rng.Intn(3); i < n; i++ {
					add(httpMockAt(s.Add(time.Duration(rng.Intn(1000)) * time.Millisecond)))
				}
			default:
				op = "close"
				if len(wins) == 0 {
					continue
				}
				x := wins[rng.Intn(len(wins))]
				x.w.Close()
				x.closed = true
			}
			drain()
			after := present()

			mgr.mu.Lock()
			var floor time.Time
			open := len(mgr.open) > 0
			if open {
				floor = mgr.open[0].start
			}
			held, claimedFirst := mgr.heldBytes, open && mgr.open[0].kept
			mgr.mu.Unlock()

			// S1
			for m := range before {
				if after[m] || sent[m] {
					continue
				}
				if open && !m.Spec.ReqTimestampMock.Before(floor) {
					t.Fatalf("seed %d step %d (%s): a mock at +%v vanished while a window open since +%v may own it",
						seed, step, op, m.Spec.ReqTimestampMock.Sub(base), floor.Sub(base))
				}
			}
			// S2, S3. After its yield, a window leaves what another request
			// claims, which stays (S1 checks it does not vanish while a window
			// that may own it is open) and is sent in the end, to that request
			// or back to this one (S5).
			if resolved != nil {
				left := func(m *models.Mock) bool {
					return !resolved.yield.IsZero() && m.Spec.ReqTimestampMock.After(resolved.yield) && after[m]
				}
				for m := range before {
					ts := m.Spec.ReqTimestampMock
					if !ts.Before(rStart) && !ts.After(rEnd) && !sent[m] {
						if !left(m) {
							t.Fatalf("seed %d step %d: kept resolve left a mock of its range unsent (still present: %v)", seed, step, after[m])
						}
						leftAfterYield++
						leftBehind[m] = true
					}
				}
				for _, m := range resolved.during {
					if ts := m.Spec.ReqTimestampMock; !ts.After(rEnd) && !sent[m] {
						if !left(m) {
							t.Fatalf("seed %d step %d: a claimed window was resolved without a mock made during it (at +%v, window +%v..+%v, present now %v)",
								seed, step, ts.Sub(base), rStart.Sub(base), rEnd.Sub(base), after[m])
						}
						leftBehind[m] = true
					}
				}
			}
			// S4
			if held > MaxHeldBytes && !claimedFirst {
				t.Fatalf("seed %d step %d (%s): %d bytes held, over the budget", seed, step, op, held)
			}
		}
		for _, x := range wins {
			x.w.Close()
		}
		if n := heldCount(mgr); n != 0 || mgr.OpenWindows() != 0 {
			t.Fatalf("seed %d: %d held, %d open after every window closed", seed, n, mgr.OpenWindows())
		}
		// S5: what a yielded window's resolve left is sent, once nothing is in
		// flight and the periodic flush has run: to the request that claimed
		// it, or back to the yielded window. Never dropped.
		mgr.FlushOwnedWindows()
		drain()
		for m := range leftBehind {
			if !sent[m] {
				t.Fatalf("seed %d: a mock a yielded window's resolve left (at +%v) was never sent", seed, m.Spec.ReqTimestampMock.Sub(base))
			}
		}
		byBudget += givenUpByBudget(mgr, func(yield func(*Window)) {
			for _, x := range wins {
				yield(x.w)
			}
		})
	}
	if byBudget == 0 {
		t.Fatal("no seed took the hold past its budget: the sequences no longer test the bound")
	}
	t.Logf("yielded windows' resolves left %d mocks to other requests over the seeds", leftAfterYield)
	if leftAfterYield == 0 {
		t.Fatal("no yielded window's resolve left a mock to another request: the sequences no longer test yields")
	}
}

// A request younger than the stale horizon keeps its mocks while what is held
// for it is within the budget: however many duplicates' leftovers pile up
// while it runs, up to MaxHeldBytes of them, it keeps its own mock. That much
// traffic was kept for a request in flight before there were windows (the
// in-flight tracker held a duplicate's prune back to its last 7 s).
func TestOpenWindowYoungRequestKeepsItsMocksWithinTheBudget(t *testing.T) {
	t.Parallel()
	// The kept resolve takes every mock of its window, the duplicates' debris
	// included (attribution is by time): room for all of them.
	out := make(chan *models.Mock, maxHeldMocks+16)
	mgr := openWindowManager(out, make(chan models.TestMockMapping, 16))
	start := time.Now().Add(-3 * time.Second)
	k := mgr.OpenWindow(start, nil)
	own := httpMockAt(start.Add(5 * time.Millisecond))
	mgr.AddMock(own)
	// Just under the budget of a duplicate's debris during K, pruned while K
	// runs.
	for i := 0; i < maxHeldMocks-2; i++ {
		mgr.AddMock(boundMockAt(start.Add(20*time.Millisecond + time.Duration(i)*100*time.Microsecond)))
	}
	mgr.DeleteMocksStrictlyBefore(time.Now())
	if held := heldCount(mgr); held != maxHeldMocks-1 {
		t.Fatalf("fixture: %d held, want everything K may own (%d)", held, maxHeldMocks-1)
	}
	if !k.Keep() {
		t.Fatal("a request younger than the stale horizon was given up within the hold's budget")
	}
	mgr.ResolveKept(k, start, time.Now(), "test-k", true)
	k.Close()
	if !hasMock(sentMocks(out), own) {
		t.Fatal("the young kept request was recorded without its own mock")
	}
}

// Past the budget the oldest request in flight goes first: an older request,
// for which most of what is held is held (only it may own what was made before
// the younger one started), is given up, and the younger one keeps what it
// owns. The hold is within its budget again once the older one's part is let
// go.
func TestOpenWindowGivesUpTheOldestRequestFirst(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 64)
	mgr := openWindowManager(out, make(chan models.TestMockMapping, 16))
	oldStart := time.Now().Add(-60 * time.Second)
	old := mgr.OpenWindow(oldStart, nil)
	youngStart := time.Now().Add(-3 * time.Second)
	young := mgr.OpenWindow(youngStart, nil)
	// The budget and a few more of a duplicate's debris while only the older
	// one was in flight, then the younger one's own call.
	for i := 0; i < maxHeldMocks+4; i++ {
		mgr.AddMock(boundMockAt(oldStart.Add(time.Second + time.Duration(i)*time.Millisecond)))
	}
	own := httpMockAt(youngStart.Add(5 * time.Millisecond))
	mgr.AddMock(own)
	mgr.DeleteMocksStrictlyBefore(time.Now())
	if old.Keep() {
		t.Fatal("past the budget the oldest request in flight was not given up")
	}
	if !young.Keep() {
		t.Fatal("the younger request was given up with the oldest, though letting the oldest go brought the hold within its budget")
	}
	mgr.mu.Lock()
	held := mgr.heldBytes
	mgr.mu.Unlock()
	if held > MaxHeldBytes {
		t.Fatalf("%d bytes still held, over the budget %d", held, MaxHeldBytes)
	}
	mgr.ResolveKept(young, youngStart, time.Now(), "test-young", true)
	young.Close()
	old.Close()
	if got := sentMocks(out); !hasMock(got, own) || len(got) != 1 {
		t.Fatalf("the younger request must be recorded with its own mock, and only it: sent %d", len(got))
	}
}

// Past the budget a request is given up whatever its age, once it is the
// oldest: the budget bounds all of the hold, the part younger than the stale
// horizon included (that part was never sized, and at tens of megabytes of
// droppable mocks a second it alone is more than an agent has). What only it
// could own is let go.
func TestOpenWindowYoungRequestIsGivenUpPastTheBudget(t *testing.T) {
	t.Parallel()
	mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	start := time.Now().Add(-3 * time.Second)
	k := mgr.OpenWindow(start, nil)
	// Just under the budget, all of it younger than the horizon: kept.
	for i := 0; i < maxHeldMocks-5; i++ {
		mgr.AddMock(boundMockAt(start.Add(20*time.Millisecond + time.Duration(i)*100*time.Microsecond)))
	}
	mgr.DeleteMocksStrictlyBefore(time.Now())
	if !k.Claim() {
		t.Fatal("a request under the budget and younger than the horizon was given up")
	}
	mgr.mu.Lock()
	k.kept = false // Claim pins the window; hand it back
	mgr.noteOldestLocked()
	held := mgr.heldBytes
	mgr.mu.Unlock()
	if held > MaxHeldBytes || held < MaxHeldBytes*9/10 {
		t.Fatalf("fixture: %d bytes held, want just under the budget %d", held, MaxHeldBytes)
	}
	// Past it: given up, though it is younger than the horizon.
	for i := 0; i < 2*maxHeldMocks/16; i++ {
		mgr.AddMock(boundMockAt(start.Add(2*time.Second + time.Duration(i)*100*time.Microsecond)))
	}
	mgr.DeleteMocksStrictlyBefore(time.Now())
	if k.Keep() {
		t.Fatal("the hold passed its budget and the oldest request was not given up")
	}
	k.Close()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.heldBytes != 0 || len(mgr.held) != 0 {
		t.Fatalf("%d bytes in %d mocks still held with no request in flight", mgr.heldBytes, len(mgr.held))
	}
}

// The left-out warning is sampled: the first, then every 1024th. A loss taken
// back (Replaced) takes the count of losses down again, and must not make the
// next loss a first: an endpoint that is left out, then recorded, over and
// over would warn every time.
func TestLeftOutWarningStaysSampledWhenLossesAreTakenBack(t *testing.T) {
	t.Parallel()
	mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	core, logs := observer.New(zap.WarnLevel)
	mgr.SetLogger(zap.New(core))
	for i := 0; i < 5; i++ {
		start := time.Now().Add(-30 * time.Second)
		w := mgr.OpenWindow(start, nil)
		for j := 0; j < maxHeldMocks+5; j++ {
			mgr.AddMock(boundMockAt(start.Add(time.Second + time.Duration(j)*time.Microsecond)))
		}
		n := time.Now()
		mgr.ResolveRange(n, n, "test-n", true, true)
		if w.Keep() {
			t.Fatal("fixture: the window was not given up")
		}
		w.Close()
		w.Replaced() // a later request of its kind was recorded
	}
	warns := logs.FilterMessageSnippet("left a kept request out").All()
	if len(warns) != 1 {
		t.Fatalf("%d left-out warnings for 5 losses, each taken back: want the first only", len(warns))
	}
	if got := mgr.KeptRequestsLeftOut(); got != 0 {
		t.Fatalf("KeptRequestsLeftOut = %d after every loss was taken back", got)
	}
}

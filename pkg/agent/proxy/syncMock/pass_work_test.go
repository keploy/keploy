package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestAResolveDecidesOnlyWhatItsWindowOrTheCutoffCanMove is the lock stall
// after an app stall: a full ring of 8192 resolved windows, and 10,000
// buffered mocks no window owns, as the mocks of requests still in flight are
// (the row opens no window: only the passes' work is counted). Each resolve
// used to look every buffered mock up in every window of the ring, all under
// mu: 10,000 × 8,192 window checks a resolve, every resolve. Now the first
// pass decides each mock once, with a search of a few dozen of the index's
// nodes, and a later pass decides only the mocks added since, those its window
// holds and those the cutoff reaches; a flush decides none. What a flush, a
// prune and a memory-pressure wipe keep stays settled: the pass after them
// decides none of it. Counted in decisions and nodes looked at
// (passWorkLocked), not in time.
func TestAResolveDecidesOnlyWhatItsWindowOrTheCutoffCanMove(t *testing.T) {
	out := make(chan *models.Mock, 1<<14)
	mgr := openWindowManager(out, make(chan models.TestMockMapping, 1<<10))
	s0 := time.Now().Add(time.Hour)
	ms := func(i int) time.Time { return s0.Add(time.Duration(i) * time.Millisecond) }
	for i := range maxRecentWindows {
		mgr.ResolveRange(ms(i), ms(i).Add(500*time.Microsecond), "r", true, false)
	}
	// One mock in the gap after each window, and the rest after the ring.
	const buffered = 10000
	for i := range buffered {
		mgr.AddMock(httpMockAt(ms(i).Add(700 * time.Microsecond)))
	}
	work := func() (uint64, uint64) {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return mgr.passWorkLocked()
	}
	// A search over a ring of 8192 looks at a few dozen nodes: at most this
	// many each, where the walk looked at all 8192.
	const perSearch = 96
	far := ms(10 * buffered)
	pass := func(what string, resolve func(), wantDecided uint64) {
		t.Helper()
		d0, v0 := work()
		resolve()
		d1, v1 := work()
		if d1-d0 != wantDecided {
			t.Fatalf("%s decided %d buffered mocks, want %d (%d buffered)", what, d1-d0, wantDecided, buffered)
		}
		if v1-v0 > perSearch*max(wantDecided, 1) {
			t.Fatalf("%s looked at %d windows of the ring for %d mocks, want at most %d a mock (the walk of the ring looks at %d a mock)", what, v1-v0, d1-d0, perSearch, maxRecentWindows)
		}
		t.Logf("%s: %d decided, %d windows looked at", what, d1-d0, v1-v0)
	}
	resolveAt := func(at time.Time, name string) func() {
		return func() { mgr.ResolveRange(at, at.Add(time.Millisecond), name, true, false) }
	}
	pass("the first resolve over the buffer", resolveAt(far, "a"), buffered)
	mgr.AddMock(httpMockAt(ms(buffered + 5)))
	pass("the next resolve", resolveAt(far.Add(time.Second), "b"), 1)
	pass("a flush", mgr.FlushOwnedWindows, 0)
	mgr.AddMock(httpMockAt(ms(buffered + 6)))
	pass("a flush over a mock added since", mgr.FlushOwnedWindows, 1)
	pass("the resolve after that flush", resolveAt(far.Add(2*time.Second), "c"), 0)
	// A prune whose horizon is before every buffered mock, and a wipe whose
	// pause holds no buffered mock's request, keep them all, settled.
	pass("a prune that keeps every mock", func() { mgr.DeleteMocksStrictlyBefore(ms(-1)) }, 0)
	pass("the resolve after the prune", resolveAt(far.Add(3*time.Second), "d"), 0)
	pass("a wipe that keeps every mock", func() {
		mgr.SetMemoryPressure(true)
		mgr.SetMemoryPressure(false)
	}, 0)
	pass("the resolve after the wipe", resolveAt(far.Add(4*time.Second), "e"), 0)
	// A window over the first 100 decides those 100, and hands them on.
	sentMocks(out)
	pass("a resolve over 100 of them", func() { mgr.ResolveRange(ms(0), ms(99).Add(800*time.Microsecond), "f", true, false) }, 100)
	if n := len(sentMocks(out)); n != 100 {
		t.Fatalf("the resolve over 100 buffered mocks handed on %d", n)
	}
}

// TestEveryResolveOverABufferSaysWhatItDecidedAndHowLongItHeldTheLock: under
// --debug, a resolve's buffer-transition line says how many buffered mocks it
// decided (decided) and how long it held mu (mu_held), for every resolve that
// passed over a buffer, those that changed nothing among them. The first pass
// over three new mocks decides and keeps all three, the next decides none of
// them, one more added is all the pass after it decides, and a window over one
// of them decides that one and hands it on. mu_held is the hold of the lock,
// within the time the call took as its caller measured it.
func TestEveryResolveOverABufferSaysWhatItDecidedAndHowLongItHeldTheLock(t *testing.T) {
	out := make(chan *models.Mock, 64)
	m := openWindowManager(out, make(chan models.TestMockMapping, 64))
	core, logs := observer.New(zap.DebugLevel)
	m.SetLogger(zap.New(core))
	base := time.Now().Add(time.Hour)
	for i := range 3 {
		m.AddMock(httpMockAt(base.Add(time.Duration(i) * 10 * time.Millisecond)))
	}
	far := base.Add(time.Hour)
	// The counts a resolve's line says: the buffer it passed over, the mocks
	// of it it decided, and those it handed on.
	type said struct{ before, decided, flushed int64 }
	resolve := func(name string, start, end time.Time, want said) {
		t.Helper()
		began := time.Now()
		m.ResolveRange(start, end, name, true, false)
		took := time.Since(began)
		var line map[string]interface{}
		lines := 0
		for _, e := range logs.FilterMessage("diag/ResolveRange: buffer transition").All() {
			if e.ContextMap()["test_name"] == name {
				line = e.ContextMap()
				lines++
			}
		}
		if lines != 1 {
			t.Fatalf("the resolve of %s wrote %d buffer-transition lines, want 1", name, lines)
		}
		num := func(k string) int64 {
			t.Helper()
			v, ok := line[k].(int64)
			if !ok {
				t.Fatalf("the resolve of %s's buffer-transition line has no %s: %v", name, k, line)
			}
			return v
		}
		if got := (said{num("buffer_len_before"), num("decided"), num("mocks_flushed")}); got != want {
			t.Fatalf("the resolve of %s said buffer_len_before, decided, mocks_flushed = %v, want %v", name, got, want)
		}
		if held, ok := line["mu_held"].(time.Duration); !ok || held < 0 || held > took {
			t.Fatalf("the resolve of %s said mu_held %v (%T), want the hold of the lock, within the %v the call took", name, line["mu_held"], line["mu_held"], took)
		}
	}
	resolve("first", far, far, said{3, 3, 0})
	resolve("second", far.Add(time.Second), far.Add(time.Second), said{3, 0, 0})
	m.AddMock(httpMockAt(base.Add(30 * time.Millisecond)))
	resolve("third", far.Add(2*time.Second), far.Add(2*time.Second), said{4, 1, 0})
	resolve("fourth", base.Add(5*time.Millisecond), base.Add(15*time.Millisecond), said{4, 1, 1})
	if n := len(sentMocks(out)); n != 1 {
		t.Fatalf("the window over one buffered mock handed on %d, want it", n)
	}
}

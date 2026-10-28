package learn

import (
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The handlers of a set the size of api-server's round under keploy mock
// record (14 813 recordings) read the offset rule's places of the set once
// and share them: a handler keeps, for the whole call and while it waits for
// another's read (derive), no copy of the set's places, only its view of
// them, a few bytes a recording. Each once kept an index of its own of every
// earlier place of the set: 93 pooled handlers in the learn step at once
// held 3.4-4.7 GB of them on api-server's round. N handlers holding their
// places at once grow the heap by at most N times (12 bytes a recording and
// 64 KiB), whatever N is, past what the set holds once.
func TestHandlersOfAnApiServerSizeSetHoldItsOffsetPlacesOnceAndTwelveBytesARecordingEach(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 14 813 recordings")
	}
	const n, handlers = 14813, 32
	const perHandler = 12*n + 64<<10
	row := newHeldRow(n, 8)
	raw := row.raw()
	heap := func() int64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	// The set's first call: what the set keeps of its recordings, read once.
	row.classify(dc)
	held := make([]*offsetPlaces, handlers)
	var wg sync.WaitGroup
	for h := range held {
		wg.Add(1)
		go func() {
			defer wg.Done()
			held[h] = dc.offsetPlacesOf(eRecAt, nil, raw, nil)
		}()
	}
	wg.Wait()
	// What the handlers hold of the offset rule's places, at once: the heap
	// while each holds its own, less the heap once none does.
	with := heap()
	runtime.KeepAlive(held)
	clear(held)
	grew := with - heap()
	t.Logf("%d handlers holding the offset rule's places of %d recordings grew the heap %d KiB (%d B a handler)", handlers, n, grew>>10, grew/handlers)
	if bound := int64(handlers*perHandler + 4<<20); grew > bound {
		t.Errorf("%d handlers holding the offset rule's places of %d recordings grew the heap %d KiB, past %d KiB (each 12 bytes a recording and 64 KiB, and 4 MiB)", handlers, n, grew>>10, bound>>10)
	}
}

// The set's handlers, at once, read each recording's places for the offset
// rule once for the set, whichever of them reads it first, through the
// offset rule's reading of a changed instant (offsetOfValue), and each learns
// what a call that read them alone learns.
func TestTheSetsHandlersReadEachRecordingsOffsetPlacesOnce(t *testing.T) {
	const n, handlers = 400, 16
	row := newHeldRow(n, 4)
	want := row.classify(noSet(NewDriftCache()))
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	got := make([][]Learned, handlers)
	var wg sync.WaitGroup
	for h := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each handler's own reading of the set's caches, as a zone's
			// is (InZone): the row sets the application's clock on it.
			dc := *tr.Drifts()
			got[h] = row.classify(&dc)
		}()
	}
	wg.Wait()
	for h, g := range got {
		if !reflect.DeepEqual(g, want) {
			t.Errorf("handler %d learned %+v, want %+v", h, g, want)
		}
	}
	ix := tr.derived.offsets
	if ix.views < handlers {
		t.Fatalf("%d handlers read the set's offset index %d times: the offset rule did not read it", handlers, ix.views)
	}
	if ix.reads != n {
		t.Errorf("%d handlers read %d recordings' offset places %d times, want each once", handlers, n, ix.reads)
	}
}

// A recording another call of the set read into the set's offset index is
// a place of a call only where that call reads it, and as it reads it: a
// call that has no payload of the receive F delivered, or of the receive the
// recording gave, or one whose payload of it says F did not serve it, learns
// what it learns with no set, before and after a call that read both; never
// refused as built on one it does not read. So does a call past the index's
// budget, which reads its recordings into an index of its own, the set's
// staying within its budget.
func TestARecordingAnotherCallOfTheSetReadIsReadOnlyAsTheCallHandsIt(t *testing.T) {
	receive, delivered := offReceives()
	const p = "body.visible_after"
	rec, live := strLeaf(p, offSent.Add(5*time.Minute).Format("2006-01-02T15:04Z07:00")), strLeaf(p, offGot.Add(5*time.Minute).Format("2006-01-02T15:04Z07:00"))
	calls := map[string]func(dc *DriftCache) []Raw{
		"both, F delivered mock-11": func(dc *DriftCache) []Raw { return []Raw{dc.RawOf(receive, false), dc.RawOf(delivered, true)} },
		"no mock-11":                func(dc *DriftCache) []Raw { return []Raw{dc.RawOf(receive, false)} },
		"mock-11 not served":        func(dc *DriftCache) []Raw { return []Raw{dc.RawOf(receive, false), dc.RawOf(delivered, false)} },
		"only mock-11, delivered":   func(dc *DriftCache) []Raw { return []Raw{dc.RawOf(delivered, true)} },
	}
	learn := func(dc *DriftCache, call string) Learned {
		return ClassifyIn(dc, []string{p}, live, rec, r1Insert, nil, calls[call](dc), nil, nil)[0]
	}
	defer func(b int64) { offsetBudget = b }(offsetBudget)
	for _, budget := range []int64{offsetBudget, 1, 600, 1200} {
		offsetBudget = budget
		for _, order := range [][]string{
			{"both, F delivered mock-11", "no mock-11", "mock-11 not served", "only mock-11, delivered"},
			{"no mock-11", "both, F delivered mock-11", "mock-11 not served", "no mock-11"},
			{"only mock-11, delivered", "mock-11 not served", "both, F delivered mock-11", "no mock-11", "only mock-11, delivered"},
		} {
			dc := clockedDrifts()
			dc.set = newSetDerived()
			calls := 0
			for _, call := range order {
				calls++
				want := learn(noSet(clockedDrifts()), call)
				got := learn(dc, call)
				if ix := dc.set.offsets; ix.offsetBytes() > budget {
					t.Errorf("budget %d, %s: the set's offset index counts %d bytes", budget, strings.Join(order[:calls], " > "), ix.offsetBytes())
				}
				if got.Kind != want.Kind || got.Why != want.Why || got.From != want.From {
					t.Errorf("budget %d, %s: %s (%s); want %s (%s), as with no set", budget, strings.Join(order[:calls], " > "), got.Kind, got.Why, want.Kind, want.Why)
				}
				if call == "both, F delivered mock-11" && !strings.Contains(got.Why, "mock-11 at spec.resp.body") {
					t.Errorf("budget %d, %s: %s (%s); want it refused, built on mock-11", budget, strings.Join(order[:calls], " > "), got.Kind, got.Why)
				}
			}
		}
	}
}

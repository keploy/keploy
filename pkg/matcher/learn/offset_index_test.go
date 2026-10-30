package learn

import (
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"sort"
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

// scanOffsetFrom is offsetFrom as it read the places before it searched
// from the deviating recordings' alone: each place of every list the call
// reads as the recording gave it, against every place within the value's
// move of it; the rows hold offsetFrom to it.
func scanOffsetFrom(op *offsetPlaces, pairs []driftedInstant) (offsetMatch, bool, int) {
	if op.unread != "" && len(pairs) > 0 {
		return offsetMatch{unread: op.unread}, true, 0
	}
	type list struct {
		k    offsetKey
		refs []offsetRef
	}
	byKey := map[offsetKey]*list{}
	var lists []*list
	for r, rec := range op.snap.recs {
		for i, kp := range rec.places {
			l := byKey[kp.k]
			if l == nil {
				l = &list{k: kp.k}
				byKey[kp.k] = l
				lists = append(lists, l)
			}
			l.refs = append(l.refs, offsetRef{rec: int32(r), i: int32(i)})
		}
	}
	sort.Slice(lists, func(a, b int) bool { return keyLess(lists[a].k, lists[b].k) })
	for _, l := range lists {
		sort.SliceStable(l.refs, func(a, b int) bool { return op.snap.momentOf(l.refs[a]) < op.snap.momentOf(l.refs[b]) })
	}
	read := 0
	for _, p := range pairs {
		for _, g := range op.given {
			if m, ok := offsetBetween(p, g[0], g[1]); ok {
				return m, true, read
			}
		}
		for _, l := range lists {
			refs, moved := l.refs, op.moves[l.k]
			for i := range refs {
				read++
				hr, ok := op.placeOf(refs[i])
				if !ok || !hr.recorded {
					continue
				}
				from, to, mfrom, mto := 0, len(refs), 0, len(moved)
				if !p.rec.key.day {
					d := p.live.key.at - p.rec.key.at
					lo, hi := hr.m.key.at+d-int64(p.rec.res), hr.m.key.at+d+int64(p.live.res)
					from = sort.Search(len(refs), func(j int) bool { return op.snap.momentOf(refs[j]) > lo })
					to = sort.Search(len(refs), func(j int) bool { return op.snap.momentOf(refs[j]) >= hi })
					mfrom = sort.Search(len(moved), func(j int) bool { return moved[j].m.key.at > lo })
					mto = sort.Search(len(moved), func(j int) bool { return moved[j].m.key.at >= hi })
				}
				for j := from; j < to; j++ {
					read++
					hf, ok := op.placeOf(refs[j])
					if !ok || offsetPass(hr, hf) {
						continue
					}
					if m, ok := offsetBetween(p, hr, hf); ok {
						return m, true, read
					}
				}
				for j := mfrom; j < mto; j++ {
					read++
					if offsetPass(hr, moved[j]) {
						continue
					}
					if m, ok := offsetBetween(p, hr, moved[j]); ok {
						return m, true, read
					}
				}
			}
		}
	}
	return offsetMatch{}, false, read
}

// randomOffsetPlaces is a call's offset places of a random set: recordings
// of two calls, taken into the index in batches (as a set's handlers take
// them), recorded around the call's instant and at it, with places at three
// keys (one of times of day), each read by the call or not, given by F or
// not, served in any order, known or not; and moves F served at the keys.
func randomOffsetPlaces(rng *rand.Rand) *offsetPlaces {
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	keys := []offsetKey{{path: "a.at", layout: 1}, {path: "b.at", layout: 1}, {path: "c.tod", layout: 2}}
	calls := []string{"find apps", "find suites"}
	moment := func(k offsetKey) moment {
		if k.layout == 2 {
			return moment{key: instantKey{at: int64(rng.Intn(40)), day: true}, res: 1}
		}
		return moment{key: instantKey{at: int64(rng.Intn(60))}, wall: int64(rng.Intn(40)), res: 1}
	}
	n := 1 + rng.Intn(14)
	srcs := make([]offsetSource, n)
	for i := range srcs {
		x := &Raw{mock: fmt.Sprintf("mock-%d", rng.Intn(n+2)), call: calls[rng.Intn(len(calls))], at: at.Add(time.Duration(rng.Intn(7)-4) * time.Second)}
		places := make([]keyedPlace, 1+rng.Intn(3))
		for j := range places {
			k := keys[rng.Intn(len(keys))]
			places[j] = keyedPlace{k: k, m: moment(k), where: fmt.Sprintf("%s at %s", x.mock, k.path)}
		}
		srcs[i] = offsetSource{key: offsetKeyOf(x.mock, x.at) + fmt.Sprint(i), x: x, places: places, kept: true, read: true}
	}
	ix := newOffsetIndex(0)
	var snap offsetSnap
	for from := 0; from < n; {
		to := min(n, from+1+rng.Intn(5))
		snap, _, _ = ix.take(nil, srcs[from:to])
		from = to
	}
	op := &offsetPlaces{at: at, snap: snap, view: make([]offsetView, len(snap.recs))}
	for r := range op.view {
		v := &op.view[r]
		v.in, v.given, v.noTime = rng.Intn(6) > 0, rng.Intn(3) > 0, rng.Intn(8) == 0
		if v.given && rng.Intn(5) > 0 {
			v.first = int32(1 + rng.Intn(20))
			v.last = v.first + int32(rng.Intn(3))*int32(rng.Intn(6))
		}
	}
	for range rng.Intn(3) {
		k := keys[rng.Intn(len(keys))]
		if op.moves == nil {
			op.moves = map[offsetKey][]offsetPlace{}
		}
		op.moves[k] = append(op.moves[k], offsetPlace{m: moment(k), mock: "moved", where: "moved at " + k.path, call: calls[rng.Intn(len(calls))], given: true, at: at})
	}
	for _, list := range op.moves {
		sort.SliceStable(list, func(i, j int) bool { return list[i].m.key.at < list[j].m.key.at })
	}
	return op
}

// The offset rule searches from the places of the recordings that deviate
// (deviating) alone, and finds, in every set, the pair the search of every
// place of the call's lists found first: the same places, the same offset,
// the same reason; and none where it found none.
func TestTheOffsetSearchFromTheDeviatingRecordingsFindsThePairTheScanOfEveryPlaceFound(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	found := 0
	for c := 0; c < 40000; c++ {
		op := randomOffsetPlaces(rng)
		var pairs []driftedInstant
		for range 1 + rng.Intn(2) {
			p := driftedInstant{rec: moment{key: instantKey{at: int64(rng.Intn(60))}, res: time.Duration(1 + rng.Intn(3))}}
			if rng.Intn(5) == 0 {
				p.rec.key = instantKey{at: int64(rng.Intn(40)), day: true}
			}
			p.live = p.rec
			p.live.key.at += int64(rng.Intn(31) - 15)
			p.live.res = time.Duration(1 + rng.Intn(3))
			pairs = append(pairs, p)
		}
		want, wantOK, _ := scanOffsetFrom(op, pairs)
		got, ok := op.offsetFrom(pairs)
		if ok != wantOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("set %d: offsetFrom found %v %+v; the scan of every place found %v %+v", c, ok, got, wantOK, want)
		}
		if ok {
			found++
		}
	}
	if found < 4000 {
		t.Fatalf("only %d of 40000 sets held a pair: the row reads too few", found)
	}
}

// A call of a set both runs gave the same recordings, in the recording's
// order, reads no place of the set's offset index: the search read every
// place each call reads, so a set's calls read its places a number of times
// that grew as the square of its size (25% of the learn handlers' CPU on
// api-server's round, on a 2-CPU agent). One recording F served out of the
// recording's order is read, with the places its move reaches, whatever the
// set's size, and the pair it is in is found as the scan found it.
func TestACallOfAnInOrderSetReadsNoPlaceOfItsOffsetIndex(t *testing.T) {
	for _, n := range []int{200, 2000} {
		row := newHeldRow(n, 1)
		raw := row.raw()
		dc := clockedDrifts()
		dc.set = newSetDerived()
		pairs := driftedInstants("2026-10-24T09:55:53.9Z", "2026-10-24T09:56:54.1Z")
		if len(pairs) == 0 {
			t.Fatal("the row's clock read holds no changed instant")
		}
		op := dc.offsetPlacesOf(eRecAt, nil, raw, nil)
		_, ok := op.offsetFrom(pairs)
		if ok || op.examined != 0 {
			t.Errorf("%d recordings both runs gave in the recording's order: the search found %v, reading %d places; want none, reading none", n, ok, op.examined)
		}
		if _, _, scan := scanOffsetFrom(op, pairs); scan < n {
			t.Fatalf("%d recordings: the scan of every place read %d, fewer than the set's: the row reads no set", n, scan)
		}
		// Two recordings of the middle of the set F served each in the
		// other's place (out of the recording's order).
		k := n / 2
		raw[k], raw[k+1] = raw[k].ServedAs(k+2), raw[k+1].ServedAs(k+1)
		op = dc.offsetPlacesOf(eRecAt, nil, raw, nil)
		m, ok := op.offsetFrom(pairs)
		want, wantOK, _ := scanOffsetFrom(op, pairs)
		if ok != wantOK || !reflect.DeepEqual(m, want) {
			t.Errorf("%d recordings, one out of order: found %v %+v, want %v %+v", n, ok, m, wantOK, want)
		}
		if bound := 2*len(op.deviating()) + 16; op.examined > bound {
			t.Errorf("%d recordings, one out of order: the search read %d places, past %d (%d deviate)", n, op.examined, bound, len(op.deviating()))
		}
	}
}

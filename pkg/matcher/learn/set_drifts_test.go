package learn

import (
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// setDriftValues are drifted values of each kind the source search reads at
// an earlier place: ids, clock reads the lead moved (of a zone and of none,
// one across New York's spring forward, a read the lead moved there and
// not in UTC), escaped and encoded text, a JWT, a UUID
// v7, an id inside text of another script, and two values whose sides,
// joined, are one text.
var setDriftValues = [][2]string{
	{"o-1", "o-1-9"},
	{"ab", "c"},
	{"a", "bc"},
	{"order 7f3a9c", "order 9b2e41"},
	{"2026-10-30T10:00:00Z", "2026-10-30T10:01:01Z"},
	{"2026-10-30T10:00:00.123Z", "2026-10-30T10:01:01.668Z"},
	{"2026-03-08 01:59:30", "2026-03-08 03:00:31"},
	{"2026-11-01 05:59:30", "2026-11-01 06:00:31"},
	{"%2Fuploads%2F7f3a9c", "%2Fuploads%2F9b2e41"},
	{`{\"id\":\"a1\"}`, `{\"id\":\"b2\"}`},
	{"eyJhbGciOiJIUzI1NiJ9.eyJpYXQiOjE3OTIwMDAwMDB9.x", "eyJhbGciOiJIUzI1NiJ9.eyJpYXQiOjE3OTIwMDAwNjF9.y"},
	{"01890a5d-ac96-774b-bcce-b302099a8057", "01890a5e-9a43-7f1c-8e2d-1c2b3a4d5e6f"},
	{"Bearer tok_A7K29Q", "Bearer tok_Z1X88M"},
	{"订单A7K29Q已发货", "订单Z1X88M已发货"},
}

// A drift the set keeps is the one a test builds of the same value with
// the same lead: every value, read with each lead, twice and interleaved,
// equals what newDrift builds of it alone. So the set keeps each by all it
// depends on, a value's two sides apart. The values hold one whose drift the
// lead changes, or the row could not tell.
func TestADriftTheSetKeepsIsTheOneATestBuilds(t *testing.T) {
	leaded := false
	for _, v := range setDriftValues {
		leaded = leaded || !reflect.DeepEqual(newDrift(v[0], v[1], nil, true, time.Time{}, nil), newDrift(v[0], v[1], nil, false, time.Time{}, nil))
	}
	if !leaded {
		t.Fatal("no value's drift depends on the lead: the row cannot tell the set's drifts apart by it")
	}
	s := newSetDerived()
	for round := 0; round < 2; round++ {
		for _, lead := range []bool{true, false} {
			for _, v := range setDriftValues {
				got := s.driftOf(v[0], v[1], lead)
				if want := newDrift(v[0], v[1], nil, lead, time.Time{}, nil); !reflect.DeepEqual(got, want) {
					t.Fatalf("round %d: the set's drift of %q -> %q (lead %v) is another's: %+v, want %+v", round, v[0], v[1], lead, got, want)
				}
			}
		}
	}
	if s.drifts.used == 0 || s.usedBytes() < s.drifts.used {
		t.Fatalf("the set keeps %d bytes of drifts and counts %d in all", s.drifts.used, s.usedBytes())
	}
}

// A drift bounded by no call reads no zone, so the set keeps one for every
// zone (driftOf): each value's drift is the same in UTC, New York and
// Kolkata. With a call it is not: a wall clock of no zone written after its
// call in UTC and before it in Kolkata is a read of the clock in Kolkata
// alone, or the row could not tell.
func TestADriftBoundedByNoCallIsTheSameInEveryZone(t *testing.T) {
	var zones []*time.Location
	for _, name := range []string{"America/New_York", "Asia/Kolkata"} {
		z, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		zones = append(zones, z)
	}
	for _, lead := range []bool{true, false} {
		for _, v := range setDriftValues {
			utc := newDrift(v[0], v[1], nil, lead, time.Time{}, nil)
			for _, z := range zones {
				if d := newDrift(v[0], v[1], nil, lead, time.Time{}, z); !reflect.DeepEqual(d, utc) {
					t.Fatalf("the drift of %q -> %q at no call (lead %v) reads %s: %+v, in UTC %+v", v[0], v[1], lead, z, d, utc)
				}
			}
		}
	}
	call, v := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), [2]string{"2026-10-30 15:00:00", "2026-10-30 15:01:01"}
	if reflect.DeepEqual(newDrift(v[0], v[1], nil, true, call, nil), newDrift(v[0], v[1], nil, true, call, zones[1])) {
		t.Fatal("the row's wall clock drifts alike in UTC and in Kolkata at its call: the row cannot tell")
	}
}

// The set's tests share the drifts of its places: a later test reads the
// drift an earlier test built, not one of its own, and so does the next set
// for none of them (Tracker.NewSet lets them go), nor a cache no tracker made,
// nor a drift read in a call's window (bounded by that call), nor a set with
// no room for them.
func TestASetsTestsShareTheDriftsOfItsPlaces(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Begin()
	a := tr.Drifts()
	da, wa := a.of("o-1", "o-1-9"), a.ofIn("o-1", "o-1-9", nil, t0)
	tr.End()
	tr.Begin()
	b := tr.Drifts()
	if a == b {
		t.Fatal("the row's two tests share one drift cache")
	}
	if b.of("o-1", "o-1-9") != da {
		t.Fatal("the next test builds the drift the earlier one built again, where the set keeps it")
	}
	if b.ofIn("o-1", "o-1-9", nil, t0) == wa {
		t.Fatal("a drift read as of a call is the set's, where it is the test's own")
	}
	tr.End()
	tr.NewSet()
	tr.Begin()
	if tr.Drifts().of("o-1", "o-1-9") == da {
		t.Fatal("the next set reads the earlier set's drift")
	}
	tr.End()
	// A handler still reading the earlier set keeps none for it.
	if b.of("o-3", "o-3-9"); b.set.drifts.used != 0 || b.set.usedBytes() != 0 {
		t.Fatalf("the earlier set still keeps %d bytes of drifts (%d in all)", b.set.drifts.used, b.set.usedBytes())
	}
	if NewDriftCache().of("o-1", "o-1-9") == NewDriftCache().of("o-1", "o-1-9") {
		t.Fatal("two caches no tracker made share a drift")
	}
	old := driftBudget
	defer func() { driftBudget = old }()
	driftBudget = 0
	s := newSetDerived()
	if s.driftOf("o-1", "o-1-9", true) == s.driftOf("o-1", "o-1-9", true) {
		t.Fatal("a set with no room for drifts keeps one")
	}
	// Room for a small drift and a larger one, not both: the first is kept,
	// the larger is not, and then none is, though a small one would fit, as
	// held keeps none past its budget (else each drift past it would have
	// the set let go of its words).
	kept := func(rec, live string) int64 {
		id := "true " + strconv.Itoa(len(rec)) + " " + rec + live
		return driftBytes(newDrift(rec, live, nil, true, time.Time{}, nil)) + slotBytes + int64(len(id))
	}
	large := [2]string{strings.Repeat("o-1 ", 8), strings.Repeat("o-1-9 ", 8)}
	driftBudget = kept("o-1", "o-1-9") + kept(large[0], large[1]) - 1
	s = newSetDerived()
	a1 := s.driftOf("o-1", "o-1-9", true)
	s.driftOf(large[0], large[1], true)
	if s.drifts.used != kept("o-1", "o-1-9") {
		t.Fatalf("the set keeps %d bytes of drifts, want the first one's %d", s.drifts.used, kept("o-1", "o-1-9"))
	}
	if s.driftOf("o-1", "o-1-9", true) != a1 {
		t.Fatal("the set lets go of a drift it kept")
	}
	if s.driftOf("o-2", "o-2-9", true) == s.driftOf("o-2", "o-2-9", true) {
		t.Fatal("the set keeps a drift after one did not fit")
	}
}

// isClockOfText is isClock reading a piece's text, as it did before each
// piece's changed words were read once (changedWords).
func isClockOfText(p run, cs ...clockSet) bool {
	r, l := tokensBy(p.rec, isWord), tokensBy(p.live, isWord)
	changed := false
	for w := range r {
		if !l[w] {
			changed = true
			if !clockSide(w, true, cs) {
				return false
			}
		}
	}
	for w := range l {
		if !r[w] {
			changed = true
			if !clockSide(w, false, cs) {
				return false
			}
		}
	}
	return changed
}

// clockWordOfText is clockWord reading the other side's words of each of
// d's forms, as it did before it read d's words (recTok, liveTok).
func clockWordOfText(t string, recorded bool, d *valueDrift, cs ...clockSet) bool {
	other := map[string]bool{}
	for _, f := range d.forms {
		side := f[1]
		if !recorded {
			side = f[0]
		}
		for w := range tokensBy(side, isWord) {
			other[w] = true
		}
	}
	ws := tokensBy(t, isWord)
	if len(ws) == 0 {
		return false
	}
	for w := range ws {
		if !other[w] && !clockSide(w, recorded, cs) {
			return false
		}
	}
	return true
}

// A piece's changed words, read once as its drift is built, say what its
// text says: isClock and clockWord answer for each piece and each changed
// token of each value, with each value's clock reads and with another's,
// as they did reading the text on each call. The values hold pieces that
// are clock reads and pieces that are not, or the row could not tell.
func TestAPiecesWordsSayWhatItsTextSays(t *testing.T) {
	var ds []*valueDrift
	for _, lead := range []bool{true, false} {
		for _, v := range setDriftValues {
			ds = append(ds, newDrift(v[0], v[1], nil, lead, time.Time{}, nil))
		}
	}
	answers := map[bool]int{}
	for _, v := range ds {
		for _, e := range ds {
			for i, p := range v.pieces {
				got, want := isClock(v.pieceWords[i], v.clock, e.clock), isClockOfText(p, v.clock, e.clock)
				if got != want {
					t.Fatalf("piece %q -> %q of %q -> %q, with %q -> %q's clock: isClock %v, its text says %v", p.rec, p.live, v.rec, v.live, e.rec, e.live, got, want)
				}
				answers[got]++
			}
			for r := range readings {
				for side, ts := range []map[string]bool{v.lost[r], v.gained[r]} {
					for tok := range ts {
						if got, want := clockWord(tok, side == 0, v, v.clock, e.clock), clockWordOfText(tok, side == 0, v, v.clock, e.clock); got != want {
							t.Fatalf("token %q of %q -> %q (recorded %v): clockWord %v, its text says %v", tok, v.rec, v.live, side == 0, got, want)
						}
					}
				}
			}
		}
	}
	if answers[true] == 0 || answers[false] == 0 {
		t.Fatalf("the values' pieces are all clock reads or none (%v): the row cannot tell", answers)
	}
}

// A set's drifts are within its budget (no-OOM): one read of the run's places
// as the last test of a set of n tests learns its changed value allocates
// a bounded number of bytes for each place, its first read (each place's
// drift is the set's, read by the earlier tests) as its later ones, at 2000
// places and at 20000; the set keeps at most its budget; and the learning is
// the one a cache of no set's reads.
func TestOneReadOfTheRunsPlacesAllocatesABoundPerPlace(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(1_792_000_000, 0)
	call := t0.Add(time.Hour)
	// perPlace is one place's copies (the learn step's, in time order) and
	// its entry in the test's own cache, with room: a read that built a
	// place's drift or its pieces' words again took 5 to 20 KiB a place.
	// fixed is the read's own.
	const perPlace, fixed = 1600, 256 << 10
	for _, n := range []int{2000, 20000} {
		tr := NewTracker()
		for i := 0; i < n; i++ {
			tr.Begin()
			k, _, _ := tr.State()
			tr.Served(k, ranCall("mock-"+strconv.Itoa(i), t0.Add(time.Duration(i)*time.Millisecond), "o-"+strconv.Itoa(i), "o-"+strconv.Itoa(i)+"-9"))
			tr.End()
		}
		// An earlier test reads the places, as the set's tests read the
		// places before each of them.
		tr.Begin()
		_, earlier, _, _ := tr.Test()
		dc := tr.Drifts()
		dc.appClock = true
		ClassifyIn(dc, []string{"body.order"}, strLeaf("body.order", "o-x-9"), strLeaf("body.order", "o-x"), call, earlier, nil, nil, nil)
		tr.End()
		tr.Begin()
		dc = tr.Drifts()
		dc.appClock = true
		for _, read := range []string{"first", "later"} {
			_, earlier, _, _ := tr.Test()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			ln := ClassifyIn(dc, []string{"body.order"}, strLeaf("body.order", "o-x-9"), strLeaf("body.order", "o-x"), call, earlier, nil, nil, nil)
			runtime.ReadMemStats(&after)
			t.Logf("%d places: the %s read allocates %d bytes (%d a place); the set keeps %d bytes", n, read, after.TotalAlloc-before.TotalAlloc, (after.TotalAlloc-before.TotalAlloc)/uint64(n), tr.derived.usedBytes())
			if got, bound := after.TotalAlloc-before.TotalAlloc, uint64(perPlace*n+fixed); got > bound {
				t.Errorf("%d places: the last test's %s read allocates %d bytes, over %d (%d a place): it builds the places' drifts or their words again", n, read, got, bound, perPlace)
			}
			plain := NewDriftCache()
			plain.appClock = true
			if want := ClassifyIn(plain, []string{"body.order"}, strLeaf("body.order", "o-x-9"), strLeaf("body.order", "o-x"), call, earlier, nil, nil, nil); !reflect.DeepEqual(ln, want) {
				t.Fatalf("%d places: the %s read learns %+v with the set's drifts, %+v with none", n, read, ln, want)
			}
		}
		if s := tr.derived; s.usedBytes() > s.budget.limit {
			t.Fatalf("%d places: the set keeps %d bytes, past its budget of %d", n, s.usedBytes(), s.budget.limit)
		}
		tr.End()
	}
}

package learn

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// scanDecide is what newDecider's call decides where every place of it is
// built and read for every word (the full scan: no place left unbuilt, an
// index that names every place as read always), and where the call builds
// and reads only what the set's postings name (scan false).
func scanDecide(in Input, allScope, scan bool) ([]Decision, []string) {
	x := newReaderIndex(in.Drifts)
	defer x.release()
	dc := newDecider(in, allScope, x)
	if scan {
		pi := &placeIndex{rec: map[int32][]int32{}, live: map[int32][]int32{}, fs: &setFilters{}}
		for i := range dc.ps.all() {
			pi.always = append(pi.always, int32(i))
		}
		dc.pi = pi
	}
	return dc.decide()
}

// placeListShapes are calls of a test's shapes the lazy place list must
// decide as the full scan does: a long test's leaves booked or not; a
// trailed test; an exchange keploy could not read; a given value and a
// body that declares no media type (read always); a call made before an
// exchange's answer; and a word several exchanges hold.
func placeListShapes() map[string]Input {
	out := map[string]Input{
		"booked":   bookShape(120, 6, 40, true),
		"unbooked": bookShape(120, 6, 40, false),
	}
	trailed := bookShape(60, 4, 20, true)
	trailed.Trails = Trail{Test: trailed.Test, Mocks: []string{"mock-e1", "mock-e3", "mock-r2"}}
	out["trailed"] = trailed
	unread := bookShape(60, 4, 20, true)
	unread.Earlier[7].Unread = "keploy could not read it"
	unread.Earlier = append(unread.Earlier, Earlier{Mock: "mock-u", At: oAt.Add(-time.Second), Own: true})
	out["unread"] = unread
	// R's word glued inside a given value, and inside a body that
	// declares no media type: no posting names either place.
	given := bookShape(60, 4, 20, true)
	given.Earlier[1].Gave, given.Earlier[1].Given = true, true
	given.Earlier[1].Seen[2].Leaf.Value = "zzrAHcEraS7Qx1zz"
	out["given"] = given
	undeclared := bookShape(60, 4, 20, true)
	undeclared.Earlier[2].Seen[1].Undeclared = true
	undeclared.Earlier[2].Seen[1].Leaf.Value = "zzrAHcEraS7Qx1zz"
	out["undeclared"] = undeclared
	late := bookShape(60, 4, 20, true)
	late.Earlier[1].At = late.At.Add(time.Second)
	late.Earlier[1].Payload = oPayload("mock-e1", late.Earlier[1].At)
	out["answered after the call"] = late
	many := bookShape(60, 4, 20, true)
	for i := 2; i < 6; i++ {
		many.Earlier[i].Seen[2].Leaf.Value, many.Earlier[i].Seen[2].Live = "rAHcEraS7Qx1", fmt.Sprintf("other-%d", i)
	}
	out["held in many"] = many
	return out
}

// A call that builds only the places its words' postings name (and those
// read always) decides as the full scan does, decision for decision, at
// each call of the test, in both scopes.
func TestPlaceListDecidesAsTheFullScan(t *testing.T) {
	for name, in := range placeListShapes() {
		t.Run(name, func(t *testing.T) {
			for _, allScope := range []bool{false, true} {
				for call := range 3 {
					lazy, lf := scanDecide(in, allScope, false)
					full, ff := scanDecide(in, allScope, true)
					if len(full) == 0 {
						t.Fatalf("call %d (all scope %v) decided nothing: the row needs a decided word", call, allScope)
					}
					if !reflect.DeepEqual(lazy, full) || !reflect.DeepEqual(lf, ff) {
						t.Fatalf("call %d (all scope %v) decided apart from the full scan:\nlazy: %v %+v\nfull: %v %+v", call, allScope, lf, lazy, ff, full)
					}
				}
			}
		})
	}
}

// A call builds the places of the exchanges its words' postings name, not
// every place of its test: at ten times the exchanges, a call builds the
// same places (work, never time), and Decide over the shape decides as the
// full scan does.
func TestACallBuildsOnlyThePlacesItReads(t *testing.T) {
	const l, r, calls = 12, 50, 3
	built := func(e int) int64 {
		in := bookShape(e, l, r, true)
		b := placesBuilt.Load()
		for range calls {
			if ds := Decide(in); len(ds) == 0 {
				t.Fatalf("a call of %d exchanges decided nothing: the row needs a decided word", e)
			}
		}
		return (placesBuilt.Load() - b) / calls
	}
	small, large := built(100), built(1000)
	t.Logf("places built a call: %d of %d places at 100 exchanges, %d of %d at 1000", small, 100*(l+1), large, 1000*(l+1))
	if small != large {
		t.Errorf("a call builds %d places at 100 exchanges and %d at 1000; want the same: what its words' postings name, not its test's places", small, large)
	}
	if large == 0 || large > 2*(l+1) {
		t.Errorf("a call of 1000 exchanges of %d leaves built %d places; want those of the one exchange that holds its word (%d), at most two exchanges'", l+1, large, l+1)
	}
	// A call that reads every place, twice, and each place again by its
	// index, builds each leaf's place once.
	in := bookShape(40, 3, 5, true)
	ps, _, _ := placesOf(in)
	ps.x = newReaderIndex(in.Drifts)
	defer ps.x.release()
	b := placesBuilt.Load()
	n := 0
	for range 2 {
		for i, p := range ps.all() {
			if q := ps.at(i); q.name != p.name {
				t.Fatalf("place %d is %s by its index and %s in order", i, q.name, p.name)
			}
			n++
		}
	}
	if got := placesBuilt.Load() - b; n != 2*ps.len() || got != int64(ps.leaves) || ps.leaves != 40*4 {
		t.Errorf("reading %d places twice in order and by index built %d of %d leaves' places (read %d); want each built once", ps.len(), got, ps.leaves, n)
	}
}

// The recording each place of a call is of, as mocks() names it without
// building a place (orderUnknown marks a source's places by it), is the
// one its built place names: a leaf's is its own exchange's, never a
// neighbour's, and a recording's place is its recording.
func TestPlaceListNamesEachPlacesRecordingAsItsBuiltPlaceDoes(t *testing.T) {
	in := bookShape(40, 3, 5, true)
	ps, _, _ := placesOf(in)
	ps.x = newReaderIndex(in.Drifts)
	defer ps.x.release()
	b := placesBuilt.Load()
	var named []string
	for i, m := range ps.mocks() {
		if i != len(named) {
			t.Fatalf("mocks() named place %d after %d places; want each place in order", i, len(named))
		}
		named = append(named, m)
	}
	if got := placesBuilt.Load() - b; got != 0 {
		t.Errorf("naming the places' recordings built %d places; want none", got)
	}
	if len(named) != ps.len() || ps.leaves != 40*4 {
		t.Fatalf("mocks() named %d places of %d (%d leaves); want every place", len(named), ps.len(), ps.leaves)
	}
	exchanges := map[string]bool{}
	for i, m := range named {
		if p := ps.at(i); p.mock != m {
			t.Fatalf("place %d (%s) is of %s as built and of %s as mocks() names it; want its own recording", i, p.name, p.mock, m)
		}
		if i < ps.leaves {
			exchanges[m] = true
		}
	}
	if len(exchanges) != 40 {
		t.Errorf("the leaves name %d exchanges; want each of the 40 its own", len(exchanges))
	}
}

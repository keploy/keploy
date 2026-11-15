package learn

import (
	"reflect"
	"slices"
	"testing"
)

// bookCall is a call's decider over places ps, read through x: what a
// row of the place index (decider.places) needs of a call.
func bookCall(x *readerIndex, ps ...evPlace) *decider { return &decider{x: x, ps: ps} }

// bookEntries is n leaves booked in b, their entries in order.
func bookEntries(b *placeBook, n int) []*leafPost {
	var out []*leafPost
	for _, s := range bookLeaves(b, make([]Seen, n)) {
		out = append(out, s.post)
	}
	return out
}

// A call's place index finds its booked leaves through the run's book: by
// a word, exactly the places whose text on the side asked the postings
// name, each place of the call once, and never a leaf the call does not
// have, whatever the book's inverse lists.
func TestPlaceBookInverseNamesTheCallsPlaces(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	dr := tr.Drifts()
	bk := &placeBook{}
	es := bookEntries(bk, 4)
	x := newReaderIndex(dr)
	// P0: alpha recorded, beta round F's; P1 the other way round; P2 a
	// second place of P0's entry.
	call := bookCall(x,
		bookPlace(x, "alpha-1 one", "beta-1 two", es[0]),
		bookPlace(x, "beta-1 two", "alpha-1 one", es[1]),
		bookPlace(x, "alpha-1 one", "beta-1 two", es[0]),
	)
	pi := call.places()
	if pi.inv == nil || len(pi.at) != 4 {
		t.Fatalf("the call's index has inverse %v and %d slots; want the book's, a slot for each of its 4 entries", pi.inv, len(pi.at))
	}
	t.Run("each side reads its own ids", func(t *testing.T) {
		if at, all := pi.recAsked("alpha"); all || !slices.Equal(at, []int32{0, 2}) {
			t.Fatalf("alpha recorded is asked at %v (all %v); want P0 and its second place P2, not P1 (alpha is its round F text)", at, all)
		}
		if at, all := pi.asked(readWord{Rec: "gamma", Live: "alpha"}); all || !slices.Equal(at, []int32{1}) {
			t.Fatalf("alpha in round F is asked at %v (all %v); want only P1", at, all)
		}
	})
	t.Run("a second place of an entry is the call's own", func(t *testing.T) {
		if pi.at[es[0].seq].i != 1 || len(pi.rec) != 1 || len(pi.live) != 1 {
			t.Fatalf("P0's slot %+v, the call's maps %v / %v; want P0 in its slot and P2 in the call's maps", pi.at[es[0].seq], pi.rec, pi.live)
		}
	})
	t.Run("a leaf listed but not in the call names nothing", func(t *testing.T) {
		// Another call lists es[2] and es[3] under alpha's ids too.
		other := bookCall(x, bookPlace(x, "alpha-1 one", "beta-1 two", es[2]), bookPlace(x, "beta-1 two", "alpha-1 one", es[3]))
		other.places()
		next := bookCall(x, bookPlace(x, "alpha-1 one", "beta-1 two", es[0]))
		if at, all := next.places().asked(readWord{Rec: "alpha", Live: "alpha"}); all || !slices.Equal(at, []int32{0}) {
			t.Fatalf("a call of P0 alone is asked at %v (all %v); want [0]: the book lists 3 more leaves of alpha, none of this call", at, all)
		}
	})
	t.Run("a leaf booked after the call's index is found", func(t *testing.T) {
		later := bookEntries(bk, 2)
		c := bookCall(x, bookPlace(x, "alpha-1 one", "beta-1 two", es[0]), bookPlace(x, "alpha-1 one", "beta-1 two", later[0]))
		c.places()
		// The book now lists later[0] under alpha too, numbered past the
		// first call's slots.
		if at, all := pi.recAsked("alpha"); all || !slices.Equal(at, []int32{0, 2}) {
			t.Fatalf("after a later leaf was listed under alpha, the first call's alpha recorded is asked at %v (all %v); want [0 2] as before", at, all)
		}
		c.pi = nil
		late := &leafPost{book: bk, seq: bk.n.Load() + 5}
		c.ps = append(c.ps, bookPlace(x, "alpha-1 one", "beta-1 two", late))
		if at, all := c.places().recAsked("alpha"); all || !slices.Equal(at, []int32{0, 1, 2}) {
			t.Fatalf("a call with a leaf numbered past the book is asked at %v (all %v); want all three, the last from the call's maps", at, all)
		}
	})
}

// A leaf whose texts changed is listed again under its new ids: a call
// finds it by them, and not by the old ones it is still listed under.
func TestPlaceBookInverseFollowsChangedTexts(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	dr := tr.Drifts()
	bk := &placeBook{}
	es := bookEntries(bk, 1)
	x := newReaderIndex(dr)
	if at, _ := bookCall(x, bookPlace(x, "alpha-1 one", "beta-1 two", es[0])).places().recAsked("alpha"); !slices.Equal(at, []int32{0}) {
		t.Fatalf("first texts: alpha recorded asked at %v; want [0]", at)
	}
	for _, c := range []struct{ rec, live, word, gone string }{
		{"delta-3 four", "beta-1 two", "delta", "alpha"},
		{"delta-3 four", "gamma-2 three", "gamma", "beta"},
	} {
		pi := bookCall(x, bookPlace(x, c.rec, c.live, es[0])).places()
		got, _ := pi.asked(readWord{Rec: c.word, Live: c.word})
		old, _ := pi.asked(readWord{Rec: c.gone, Live: c.gone})
		if !slices.Equal(got, []int32{0}) || len(old) != 0 {
			t.Fatalf("texts %q/%q: %s asked at %v, %s at %v; want [0] by the new texts and none by the old", c.rec, c.live, c.word, got, c.gone, old)
		}
	}
	// Another set's call lists it in an inverse of its own, though its
	// texts' ids there are the same numbers.
	tr2 := NewTracker()
	tr2.NewSet()
	x2 := newReaderIndex(tr2.Drifts())
	bookPlace(x2, "alpha-1 one", "beta-1 two", nil).postIDs(x2.filters)
	r1, l1 := bookPlace(x, "delta-3 four", "gamma-2 three", nil).postIDs(x.filters)
	if r2, l2 := bookPlace(x2, "delta-3 four", "gamma-2 three", nil).postIDs(x2.filters); r1 != r2 || l1 != l2 {
		t.Fatalf("the texts' ids are %d,%d in one set and %d,%d in the other; the row needs the same numbers", r1, l1, r2, l2)
	}
	first := bk.invFor(x.filters)
	pi2 := bookCall(x2, bookPlace(x2, "delta-3 four", "gamma-2 three", es[0])).places()
	if pi2.inv == first || pi2.inv.fs != x2.filters {
		t.Fatalf("another set's call has the inverse of set %p; want one of its own (%p)", pi2.inv.fs, x2.filters)
	}
	if at, _ := pi2.recAsked("delta"); !slices.Equal(at, []int32{0}) {
		t.Fatalf("another set: delta recorded asked at %v; want [0]", at)
	}
	pi3 := bookCall(x, bookPlace(x, "delta-3 four", "gamma-2 three", es[0])).places()
	if pi3.inv == first || pi3.inv == pi2.inv {
		t.Fatal("the first set's next call has an earlier inverse; want a new one (the book keeps one set's)")
	}
	if at, _ := pi3.recAsked("delta"); !slices.Equal(at, []int32{0}) {
		t.Fatalf("the first set again: delta recorded asked at %v; want [0]", at)
	}
}

// Over a test's calls the book lists each booked leaf once, every call
// indexes it through the book, and none of them goes in a call's maps:
// counted as work, never as time.
func TestPlaceBookIndexesLeavesWithNoMapOfTheCall(t *testing.T) {
	const e, l, r, n = 60, 4, 30, 3
	leaves := int64(e * (l + 1))
	run := func(booked bool) (ds [][]Decision, indexed, mapped, listed int64) {
		in := bookShape(e, l, r, booked)
		i0, m0, l0 := bookIndexed.Load(), placeMapped.Load(), bookListings.Load()
		for range n {
			ds = append(ds, Decide(in))
		}
		return ds, bookIndexed.Load() - i0, placeMapped.Load() - m0, bookListings.Load() - l0
	}
	plain, pIdx, pMap, pList := run(false)
	booked, bIdx, bMap, bList := run(true)
	for i := range plain {
		if len(plain[i]) == 0 || !reflect.DeepEqual(plain[i], booked[i]) {
			t.Fatalf("call %d decided apart through the book:\nno book: %v\nbook:    %v", i, plain[i], booked[i])
		}
	}
	if pIdx != 0 || pList != 0 {
		t.Errorf("with no book: %d indexed through it, %d listed; want 0", pIdx, pList)
	}
	if bIdx != n*leaves || bList != leaves {
		t.Errorf("with the book: %d places indexed through it, %d listings; want %d (each leaf, each call) and %d (each leaf once)", bIdx, bList, n*leaves, leaves)
	}
	if bMap != pMap-n*leaves {
		t.Errorf("with the book, %d places in the calls' maps; want %d (%d with no book, less every leaf of every call)", bMap, pMap-n*leaves, pMap)
	}
}

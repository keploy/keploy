package learn

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// bookShape is a long test's call over e earlier exchanges of l answer
// leaves each (and a request leaf), and r recordings, as the learn step's
// profile of a long test reads it; booked gives each exchange's leaves
// their place-book entries, as the tracker does where it keeps them.
func bookShape(e, l, r int, booked bool) Input {
	var earlier []Earlier
	for i := range e {
		at := oAt.Add(-time.Duration(e-i) * time.Millisecond)
		x := Earlier{Mock: fmt.Sprintf("mock-e%d", i), At: at, Own: true, Payload: oPayload(fmt.Sprintf("mock-e%d", i), at)}
		x.Seen = append(x.Seen, Seen{Side: Request, Leaf: oText("body.filter._id", fmt.Sprintf("65a1b2c3d4e5f6a7b8c9%04x", i)), Live: fmt.Sprintf("75a1b2c3d4e5f6a7b8c9%04x", i), Place: "body.filter._id"})
		for j := range l {
			p := fmt.Sprintf("body.cursor.firstBatch.0.f%d", j)
			x.Seen = append(x.Seen, Seen{Side: Response, Leaf: oText(p, fmt.Sprintf("val-%d-%d-rAHcEraS7Qx%d", i, j, i)), Live: fmt.Sprintf("val-%d-%d-Zq81Lm0XpV%d", i, j, i), Place: p})
		}
		if booked {
			x.Seen = bookLeaves(x.Seen)
		}
		earlier = append(earlier, x)
	}
	var raw []Raw
	for i := range r {
		name := fmt.Sprintf("mock-r%d", i)
		raw = append(raw, RawOf(getAnswer(name, "http://api/items", fmt.Sprintf(`{"v":"x%d","id":"aa%06d"}`, i, i), oAt.Add(-time.Hour)), false))
	}
	in := oInput("body.user", "rAHcEraS7Qx1", "Zq81Lm0XpVb4", earlier, raw)
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	in.Drifts = tr.Drifts()
	return in
}

// A test's calls read its exchanges' leaves' posting ids from the place
// book: the first call derives each leaf's ids, every later call reads them
// from the leaf's entry, and each call decides exactly as it does with no
// book (each call deriving every place's ids). Counted as work (ids
// derived, entries read), never as time.
func TestPlaceBookDerivesLeafIDsOncePerTest(t *testing.T) {
	const e, l, r, n = 150, 6, 100, 4
	leaves := int64(e * (l + 1))
	run := func(booked bool) (ds [][]Decision, derived, read, builds int64) {
		in := bookShape(e, l, r, booked)
		d0, r0, b0 := leafPosts.Load(), bookReads.Load(), placeBuilds.Load()
		for range n {
			ds = append(ds, Decide(in))
		}
		return ds, leafPosts.Load() - d0, bookReads.Load() - r0, placeBuilds.Load() - b0
	}
	plain, pDerived, pRead, pBuilds := run(false)
	booked, bDerived, bRead, bBuilds := run(true)
	if pBuilds != n || bBuilds != n {
		t.Fatalf("place indexes built: %d with no book, %d with it; want one per call (%d): the row asks every call's places", pBuilds, bBuilds, n)
	}
	for i := range plain {
		if len(plain[i]) == 0 {
			t.Fatalf("call %d decided nothing: the row needs a decided word", i)
		}
		if !reflect.DeepEqual(plain[i], booked[i]) {
			t.Fatalf("call %d decided apart with the place book:\nno book: %+v\nbook:    %+v", i, plain[i], booked[i])
		}
	}
	if pRead != 0 {
		t.Errorf("with no book, %d entries read; want 0", pRead)
	}
	if want := (n - 1) * leaves; bRead != want {
		t.Errorf("with the book, %d leaf entries read; want %d (every leaf, in each call after the first)", bRead, want)
	}
	if want := pDerived - (n-1)*leaves; bDerived != want {
		t.Errorf("with the book, %d places' ids derived; want %d (%d with no book, less each leaf in each call after the first)", bDerived, want, pDerived)
	}
}

// bookPlace is a place of texts rec and live read through x, with the
// leaf's book entry post (nil: none).
func bookPlace(x *readerIndex, rec, live string, post *leafPost) evPlace {
	p := evPlace{rec: rec, live: live, post: post}
	p.t = &placeText{rec: x.text(rec), live: x.text(live)}
	return p
}

// A leaf's entry gives its ids only to a call of the set it derived them in
// and for the texts it derived them for; a text the postings hold no id of
// leaves no entry, so the next call asks again.
func TestPlaceBookKeepsIDsForItsSetAndTexts(t *testing.T) {
	newSet := func() *DriftCache {
		tr := NewTracker()
		tr.NewSet()
		return tr.Drifts()
	}
	dc := newSet()
	ask := func(x *readerIndex, rec, live string, post *leafPost) (r, l int32, derived, read int64) {
		d0, r0 := leafPosts.Load(), bookReads.Load()
		r, l = bookPlace(x, rec, live, post).postIDs(x.filters)
		return r, l, leafPosts.Load() - d0, bookReads.Load() - r0
	}
	post := &leafPost{}
	x := newReaderIndex(dc)
	if x.filters == nil {
		t.Fatal("a set's call has no postings: the row needs them")
	}
	r1, l1, d, rd := ask(x, "alpha-1 one", "beta-1 two", post)
	if r1 < 0 || l1 < 0 || d != 1 || rd != 0 {
		t.Fatalf("first ask: ids %d,%d, %d derived, %d read; want both posted, derived once", r1, l1, d, rd)
	}
	t.Run("a later call of the set reads them", func(t *testing.T) {
		if r, l, d, rd := ask(newReaderIndex(dc), "alpha-1 one", "beta-1 two", post); r != r1 || l != l1 || d != 0 || rd != 1 {
			t.Fatalf("ids %d,%d, %d derived, %d read; want %d,%d read from the entry", r, l, d, rd, r1, l1)
		}
	})
	t.Run("changed texts are derived again", func(t *testing.T) {
		x := newReaderIndex(dc)
		for _, c := range []struct{ rec, live string }{{"alpha-1 one", "gamma-2 three"}, {"delta-3 four", "gamma-2 three"}} {
			want, wantL := bookPlace(x, c.rec, c.live, nil).postIDs(x.filters)
			if r, l, d, rd := ask(x, c.rec, c.live, post); r != want || l != wantL || d != 1 || rd != 0 {
				t.Fatalf("%q/%q: ids %d,%d, %d derived, %d read; want %d,%d derived", c.rec, c.live, r, l, d, rd, want, wantL)
			}
		}
		if r, l, _, rd := ask(x, "alpha-1 one", "beta-1 two", post); r != r1 || l != l1 || rd != 0 {
			t.Fatalf("back to the first texts: ids %d,%d, %d read; want %d,%d derived again", r, l, rd, r1, l1)
		}
	})
	t.Run("another set's call derives its own", func(t *testing.T) {
		other := newReaderIndex(newSet())
		want, wantL := bookPlace(other, "alpha-1 one", "beta-1 two", nil).postIDs(other.filters)
		if r, l, d, rd := ask(other, "alpha-1 one", "beta-1 two", post); r != want || l != wantL || d != 1 || rd != 0 {
			t.Fatalf("ids %d,%d, %d derived, %d read; want %d,%d derived in its set", r, l, d, rd, want, wantL)
		}
		if _, _, d, rd := ask(newReaderIndex(dc), "alpha-1 one", "beta-1 two", post); d != 1 || rd != 0 {
			t.Fatalf("the first set again: %d derived, %d read; want derived (the entry keeps the other set's)", d, rd)
		}
	})
	t.Run("a call in no set keeps nothing", func(t *testing.T) {
		kept := post.kept.Load()
		if r, l, d, rd := ask(newReaderIndex(nil), "alpha-1 one", "beta-1 two", post); r != -1 || l != -1 || d != 1 || rd != 0 || post.kept.Load() != kept {
			t.Fatalf("ids %d,%d, %d derived, %d read, entry changed %v; want -1,-1 derived, the entry as it was", r, l, d, rd, post.kept.Load() != kept)
		}
	})
	t.Run("an unposted text keeps no entry", func(t *testing.T) {
		dc := newSet()
		x := newReaderIndex(dc)
		if r, l := bookPlace(x, "alpha-1 one", "beta-1 two", nil).postIDs(x.filters); r < 0 || l < 0 {
			t.Fatalf("ids %d,%d before the budget filled; want both posted", r, l)
		}
		s := dc.set
		s.mu.Lock()
		was := s.budget.used
		s.budget.used = s.budget.limit
		s.mu.Unlock()
		post := &leafPost{}
		r, l, d, _ := ask(x, "eps-5 five", "zeta-6 six", post)
		if (r >= 0 && l >= 0) || d != 1 || post.kept.Load() != nil {
			t.Fatalf("past the set's budget: ids %d,%d, %d derived, entry %v; want one unposted, derived, no entry", r, l, d, post.kept.Load())
		}
		for _, c := range []struct{ rec, live string }{{"alpha-1 one", "eta-7 seven"}, {"theta-8 eight", "beta-1 two"}} {
			post := &leafPost{}
			r, l, d, _ := ask(x, c.rec, c.live, post)
			if (r >= 0) == (l >= 0) || d != 1 || post.kept.Load() != nil {
				t.Fatalf("%q/%q, one posted before the budget filled: ids %d,%d, %d derived, entry %v; want one posted, one not, no entry", c.rec, c.live, r, l, d, post.kept.Load())
			}
		}
		s.mu.Lock()
		s.budget.used = was
		s.mu.Unlock()
		x = newReaderIndex(dc)
		if r, l, d, rd := ask(x, "eps-5 five", "zeta-6 six", post); r < 0 || l < 0 || d != 1 || rd != 0 || post.kept.Load() == nil {
			t.Fatalf("within the budget again: ids %d,%d, %d derived, %d read; want both posted, derived and kept", r, l, d, rd)
		}
	})
	t.Run("a place with no entry derives at each ask", func(t *testing.T) {
		for range 2 {
			if _, _, d, rd := ask(newReaderIndex(dc), "alpha-1 one", "beta-1 two", nil); d != 1 || rd != 0 {
				t.Fatalf("%d derived, %d read; want derived", d, rd)
			}
		}
	})
}

// The tracker gives an exchange's leaves their book entries once, where it
// keeps the exchange: every read of its places shares them, one per leaf,
// and the adapter's own leaves are left as they were.
func TestPlaceBookOncePerExchange(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	seen := []Seen{
		{Side: Request, Leaf: oText("body.filter._id", "65a1b2c3d4e5f6a7b8c90001"), Live: "75a1b2c3d4e5f6a7b8c90001", Place: "body.filter._id"},
		{Side: Response, Leaf: oText("body.v", "val-rAHcEraS7Qx1"), Live: "val-Zq81Lm0XpVb4", Place: "body.v"},
	}
	given := []Seen{{Side: Request, Leaf: oText("header.x-id", "given-a1"), Live: "given-b2", Place: "header.x-id"}}
	tr.Served(tr.Epoch(), Earlier{Mock: "mock-1", At: oAt.Add(-time.Second), Own: true, Payload: oPayload("mock-1", oAt.Add(-time.Second)), Seen: seen})
	tr.GiveValue(Earlier{Mock: "given-1", At: oAt.Add(-2 * time.Second), Seen: given})
	asked := []Seen{{Side: Request, Leaf: oText("body.q", "asked-a1"), Live: "asked-b2", Place: "body.q"}}
	tr.Give(Earlier{Mock: "asked-1", At: oAt.Add(-3 * time.Second), Seen: asked})
	for _, s := range append(append(seen, given...), asked...) {
		if s.post != nil {
			t.Fatalf("the adapter's leaf %s got an entry: the tracker books its own copy", s.Place)
		}
	}
	posts := func() map[string]*leafPost {
		_, earlier, _, _, _ := tr.Places()
		out := map[string]*leafPost{}
		for _, e := range earlier {
			for _, s := range e.Seen {
				out[e.Mock+" "+s.Place] = s.post
			}
		}
		return out
	}
	first, again := posts(), posts()
	if len(first) != 4 {
		t.Fatalf("places read: %v; want the served exchange's two leaves, the given value's and the given request's", first)
	}
	byPost := map[*leafPost]string{}
	for k, p := range first {
		if p == nil {
			t.Fatalf("%s has no book entry", k)
		}
		if again[k] != p {
			t.Fatalf("%s: a second read gives another entry", k)
		}
		if o, dup := byPost[p]; dup {
			t.Fatalf("%s and %s share an entry", k, o)
		}
		byPost[p] = k
	}
	if b := bookLeaves(nil); b != nil {
		t.Fatalf("bookLeaves(nil) = %v; want nil", b)
	}
	if kept := bookLeaves([]Seen{{post: first["mock-1 body.v"]}}); kept[0].post != first["mock-1 body.v"] {
		t.Fatal("a leaf's entry was replaced: a leaf keeps the entry it has")
	}
}

// unbooked is earlier with its leaves' place-book entries left out (a copy):
// a row that compares a read of the tracker's places whole compares what
// the places are, not the cache of their ids each leaf carries.
func unbooked(earlier []Earlier) []Earlier {
	if earlier == nil {
		return nil
	}
	out := make([]Earlier, len(earlier))
	for i, e := range earlier {
		if e.Seen != nil {
			e.Seen = append([]Seen{}, e.Seen...)
			for j := range e.Seen {
				e.Seen[j].post = nil
			}
		}
		out[i] = e
	}
	return out
}

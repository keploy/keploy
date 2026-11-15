package learn

import (
	"sync"
	"sync/atomic"
)

// leafPost is one leaf's entry in its test's place book: the ids the set's
// postings name the leaf's recorded text and round F's by
// (readerText.postID), derived at the first call of the test that asks and
// read by each later call from the entry, which every copy of the leaf
// shares (bookLeaves). A call's place index (decider.places) asked the
// set's postings again for every place of the test at each call, so a test
// of N calls derived its leaves' ids N times: most of the learn step's work
// on a long test. The entry keeps the set and the texts it derived the ids
// for, and a call reads them only for that set and those texts: a call of
// another set, or a leaf whose texts changed since, derives its ids again
// and keeps those. The postings never let an id go nor give it to another
// text (postRuns), so a kept id names its text's runs for the rest of the
// set. A text the postings hold no id of (-1: unposted, or past the set's
// budget) keeps no entry: the next call asks again, as each call did.
//
// book and seq are the run's book the entry is in and its number there
// (placeBook), nil and 0 for an entry no tracker booked; listed is the
// book's inverse of the ids its entry is listed under (bookInv.list).
type leafPost struct {
	kept   atomic.Pointer[leafPosted]
	book   *placeBook
	seq    int32
	listed atomic.Pointer[bookListed]
}

// leafPosted is what a leafPost keeps: the set's filters the ids are of, the
// two texts, and their ids.
type leafPosted struct {
	fs        *setFilters
	rec, live string
	r, l      int32
}

// placeBook is a run's book of the leaves its tracker keeps (bookLeaves):
// each entry gets the next number in it (leafPost.seq), and inv is, for the
// postings of the set a call last asked, the inverse of the entries' ids
// (bookInv). A call's place index then finds the booked leaves that may
// hold a word through inv, by number, instead of putting each leaf in maps
// of its own at every call: those two map assigns per leaf were most of
// what a call's place index cost on a long test once each leaf's ids were
// read from its entry.
type placeBook struct {
	n   atomic.Int32
	mu  sync.Mutex
	inv *bookInv // guarded by mu
}

// bookInv is a place book's inverse for one set's postings (fs): by a
// text's id there, the numbers of the book's entries listed with it as
// their recorded text's id (rec) or round F's (live). A number listed under
// an id its entry no longer has stays (an entry's texts changed, it was
// listed again under its new ids): a call keeps only the numbers whose
// place it indexed with that id (bookAt), so a stale one names nothing.
type bookInv struct {
	fs        *setFilters
	mu        sync.Mutex
	rec, live map[int32][]int32
}

// bookListed is an entry's last listing: the inverse it is in and the ids
// it is listed under there.
type bookListed struct {
	inv  *bookInv
	r, l int32
}

// bookAt is a call's place of a booked entry, by the entry's number: its
// index in the call's places plus one (0: the call indexed no place of the
// entry through the book) and the ids the call read for it.
type bookAt struct{ i, r, l int32 }

// bookLeaves is seen copied, each leaf given an entry of its own in the
// run's place book b (leafPost): once per exchange, where the tracker keeps
// it (Served, Give, GiveValue), numbered in b in order. Every later copy of
// a leaf (Placed's Earlier, ranCalls) shares its entry. A leaf that already
// has one keeps it.
func bookLeaves(b *placeBook, seen []Seen) []Seen {
	out := append([]Seen(nil), seen...)
	n := int32(0)
	for i := range out {
		if out[i].post == nil {
			n++
		}
	}
	posts := make([]leafPost, n)
	first := b.n.Add(n) - n
	j := int32(0)
	for i := range out {
		if out[i].post == nil {
			p := &posts[j]
			p.book, p.seq = b, first+j
			j++
			out[i].post = p
		}
	}
	return out
}

// invFor is b's inverse for the set's postings fs: the one it keeps where
// it is fs's, else a new one, kept from now (a call of the other set makes
// its own again). Each call asks it once, and keeps the one it got for all
// its asks.
func (b *placeBook) invFor(fs *setFilters) *bookInv {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inv == nil || b.inv.fs != fs {
		b.inv = &bookInv{fs: fs, rec: map[int32][]int32{}, live: map[int32][]int32{}}
	}
	return b.inv
}

// list lists p's number under its ids r and l in inv, unless its last
// listing is that one: a call reads each booked leaf's ids at every call,
// but lists a leaf only once for a set and its texts' ids.
func (inv *bookInv) list(p *leafPost, r, l int32) {
	if k := p.listed.Load(); k != nil && k.inv == inv && k.r == r && k.l == l {
		return
	}
	inv.mu.Lock()
	inv.rec[r] = append(inv.rec[r], p.seq)
	inv.live[l] = append(inv.live[l], p.seq)
	inv.mu.Unlock()
	bookListings.Add(1)
	p.listed.Store(&bookListed{inv: inv, r: r, l: l})
}

// marked is ask with the call's places (at) of the entries inv lists under
// id on one side (live: round F's text, else the recorded one) added: only
// those the call indexed through the book with that id on that side.
func (inv *bookInv) marked(ask []int32, id int32, live bool, at []bookAt) []int32 {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	seqs := inv.rec[id]
	if live {
		seqs = inv.live[id]
	}
	for _, s := range seqs {
		if int(s) >= len(at) {
			continue
		}
		a := at[s]
		if a.i > 0 && (!live && a.r == id || live && a.l == id) {
			ask = append(ask, a.i-1)
		}
	}
	return ask
}

// postIDs is the ids the set's postings (fs) name p's recorded text and
// round F's by (readerText.postID), read from p's leaf's book entry where
// it keeps them for fs and these texts, else derived and kept there where
// both are posted (a call in no set posts neither: fs nil). A recording's
// place has no entry: its ids are derived.
func (p evPlace) postIDs(fs *setFilters) (r, l int32) {
	if b := p.post; b != nil {
		if k := b.kept.Load(); k != nil && k.fs == fs && k.rec == p.rec && k.live == p.live {
			bookReads.Add(1)
			return k.r, k.l
		}
	}
	r, l = p.t.rec.postID(), p.t.live.postID()
	leafPosts.Add(1)
	if p.post != nil && r >= 0 && l >= 0 {
		p.post.kept.Store(&leafPosted{fs: fs, rec: p.rec, live: p.live, r: r, l: l})
	}
	return r, l
}

// leafPosts counts the places whose ids a call's place index derived
// (postIDs), bookReads those it read from a leaf's book entry, bookIndexed
// the places it indexed through the book, placeMapped those it put in maps
// of its own, and bookListings the entries listed in a book's inverse: the
// rows' count of the work, never its time.
var leafPosts, bookReads, bookIndexed, placeMapped, bookListings atomic.Int64

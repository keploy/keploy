package learn

import "sync/atomic"

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
type leafPost struct{ kept atomic.Pointer[leafPosted] }

// leafPosted is what a leafPost keeps: the set's filters the ids are of, the
// two texts, and their ids.
type leafPosted struct {
	fs        *setFilters
	rec, live string
	r, l      int32
}

// bookLeaves is seen copied, each leaf given an entry of its own in the
// place book (leafPost): once per exchange, where the tracker keeps it
// (Served, Give, GiveValue). Every later copy of a leaf (Placed's Earlier,
// ranCalls) shares its entry. A leaf that already has one keeps it.
func bookLeaves(seen []Seen) []Seen {
	out := append([]Seen(nil), seen...)
	posts := make([]leafPost, len(out))
	for i := range out {
		if out[i].post == nil {
			out[i].post = &posts[i]
		}
	}
	return out
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
// (postIDs), bookReads those it read from a leaf's book entry: the rows'
// count of the work, never its time.
var leafPosts, bookReads atomic.Int64

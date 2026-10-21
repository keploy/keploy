package learn

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// servedIndex is what a test served so far (Tracker.Test's earlier), read
// once per exchange: each leaf by the words it holds, and each drifted leaf
// by what sources can find it by. A request reads the few leaves that can be
// a value's earlier place, not every leaf the test served before it, which
// grows with every call the test makes.
type servedIndex struct {
	mu   sync.Mutex
	list []Earlier
	// pairs are the drifted values the leaves of list hold (dc.of), each
	// once, by the order they were first served in.
	pairs  []*indexedPair
	pairOf map[*valueDrift]int32
	// What a drifted value is found by, one key for each rule of sources
	// (candidates): its recorded value whole; its wholes; the words it lost
	// and gained, by reading; the ASCII words of its recorded and its live
	// value, raw and decoded; the longest ASCII word of each of its pieces
	// (pieceRec, or pieceLive for a piece with none recorded), or else its
	// first bytes beyond ASCII (windowKey; pieceAny with none at all); every
	// stretch of bytes beyond ASCII of its recorded and its live value
	// (recWin, liveWin: windows); the longest ASCII word of a whole clock
	// read, raw and decoded (clockAny with none); and the ASCII words of the
	// text its encoded runs decode to (encAll: every value that has such
	// text).
	exact, wholes       map[string][]int32
	lost, gained        []map[string][]int32
	recWord, liveWord   map[string][]int32
	recWin, liveWin     map[string][]int32
	pieceRec, pieceLive map[string][]int32
	pieceAny            []int32
	clockRec            map[string][]int32
	clockAny            []int32
	encWord             map[string][]int32
	encAll              []int32
	// recTok and liveTok are every word of every leaf, drifted or not, by
	// reading, recorded and live (valueDrift.recTok, liveTok), as an index
	// into firsts: the earliest leaves holding it (unaccounted).
	recTok, liveTok []map[string]int32
	firsts          [][]firstsAt
}

// indexedPair is a drifted value served in the test, and the earliest leaves
// that held it, by the rank appeared gives each (classOf).
type indexedPair struct {
	d *valueDrift
	// from is the first exchange of list that held it.
	from  int32
	class [4][]firstsAt
}

// leafAt is a leaf of the index's list: its exchange's place there and its
// own in that exchange's Seen.
type leafAt struct{ ex, leaf int32 }

// noLeaf is no leaf.
var noLeaf = leafAt{-1, -1}

// firstsAt are the earliest leaves that hold a key once the exchanges of list
// before from+1 were read: the first (by when its exchange was recorded, then
// by list order, as Classify reads what was served), and the first of a mock
// other than that one's, the first a list without that mock's exchanges
// holds (a request's earlier places leave its own mock's out). A key keeps
// one each time they change, by from, so a list that is a part of the
// index's reads them too.
type firstsAt struct {
	from         int32
	first, other leafAt
}

func newServedIndex() *servedIndex {
	ix := &servedIndex{
		pairOf: map[*valueDrift]int32{}, exact: map[string][]int32{}, wholes: map[string][]int32{},
		recWord: map[string][]int32{}, liveWord: map[string][]int32{}, recWin: map[string][]int32{}, liveWin: map[string][]int32{},
		pieceRec: map[string][]int32{}, pieceLive: map[string][]int32{},
		clockRec: map[string][]int32{}, encWord: map[string][]int32{},
	}
	for range readings {
		ix.lost, ix.gained = append(ix.lost, map[string][]int32{}), append(ix.gained, map[string][]int32{})
		ix.recTok, ix.liveTok = append(ix.recTok, map[string]int32{}), append(ix.liveTok, map[string]int32{})
	}
	return ix
}

// earlierView is a request's earlier places, read in the index: the first n
// exchanges of its list recorded no later than until, without mock's when
// skip.
type earlierView struct {
	ix    *servedIndex
	n     int32
	skip  bool
	mock  string
	until time.Time
}

// view is earlier (what a request was served before it, in any order) read
// in the test's index. A cache the tracker made reads the test's exchanges
// from it, each once; a list that is not the tracker's (one read after the
// next test began) is read on its own. A cache of no tracker keeps the list
// it was last given, and reads only what a longer one adds.
func (dc *DriftCache) view(earlier []Earlier) earlierView {
	dc.ixMu.Lock()
	if dc.ix == nil {
		dc.ix = newServedIndex()
	}
	ix := dc.ix
	ix.mu.Lock()
	if dc.tracker != nil {
		if more, ok := dc.tracker.since(dc.epoch, len(ix.list)); ok {
			ix.add(dc, more)
		}
	}
	w, ok := ix.match(earlier)
	if !ok && dc.tracker == nil {
		if !ix.prefixOf(earlier) {
			ix.mu.Unlock()
			ix = newServedIndex()
			ix.mu.Lock()
			dc.ix = ix
		}
		ix.add(dc, earlier[len(ix.list):])
		w, ok = ix.match(earlier)
	}
	ix.mu.Unlock()
	dc.ixMu.Unlock()
	if ok {
		return w
	}
	ix = newServedIndex()
	ix.add(dc, earlier)
	w, _ = ix.match(earlier)
	return w
}

// sameEarlier reports whether a and b are one served exchange: a request's
// earlier places are copies of the tracker's (Tracker.Test), whose leaves
// are not changed once served (Named copies them).
func sameEarlier(a, b *Earlier) bool {
	if a.Mock != b.Mock || !a.At.Equal(b.At) || a.Own != b.Own || a.Run != b.Run || len(a.Seen) != len(b.Seen) {
		return false
	}
	return len(a.Seen) == 0 || &a.Seen[0] == &b.Seen[0]
}

// match reads earlier as the index's list up to some exchange, in its order,
// without one mock's exchanges (the paired mock's own) and those recorded
// after the last earlier one was (a request's that were recorded after it),
// if it is that (ix.mu is held).
func (ix *servedIndex) match(earlier []Earlier) (earlierView, bool) {
	w := earlierView{ix: ix}
	var left []int32
	k := 0
	for j := range earlier {
		for k < len(ix.list) && !sameEarlier(&ix.list[k], &earlier[j]) {
			left = append(left, int32(k))
			k++
		}
		if k == len(ix.list) {
			return w, false
		}
		if j == 0 || earlier[j].At.After(w.until) {
			w.until = earlier[j].At
		}
		k++
	}
	w.n = int32(k)
	for _, l := range left {
		e := &ix.list[l]
		if e.At.After(w.until) {
			continue
		}
		if w.skip && e.Mock != w.mock {
			return w, false
		}
		w.skip, w.mock = true, e.Mock
	}
	if w.skip {
		for j := range earlier {
			if earlier[j].Mock == w.mock {
				return w, false
			}
		}
	}
	return w, true
}

// prefixOf reports whether the index's list begins earlier (ix.mu is held).
func (ix *servedIndex) prefixOf(earlier []Earlier) bool {
	if len(ix.list) > len(earlier) {
		return false
	}
	for k := range ix.list {
		if !sameEarlier(&ix.list[k], &earlier[k]) {
			return false
		}
	}
	return true
}

// add reads es, served after the index's list, into it (ix.mu is held).
func (ix *servedIndex) add(dc *DriftCache, es []Earlier) {
	for _, e := range es {
		ex := int32(len(ix.list))
		ix.list = append(ix.list, e)
		for li, s := range e.Seen {
			d := dc.of(s.Leaf.Value, s.Live)
			at := leafAt{ex, int32(li)}
			for i := range readings {
				for t := range d.recTok[i] {
					ix.place(ix.firstsOf(ix.recTok[i], t), at)
				}
				for t := range d.liveTok[i] {
					ix.place(ix.firstsOf(ix.liveTok[i], t), at)
				}
			}
			// A place F saw hold its recorded value did not drift (appeared).
			if s.Live == s.Leaf.Value {
				continue
			}
			p := ix.pair(d, ex)
			ix.place(&p.class[classOf(e, s)], at)
		}
	}
}

// classOf is the rank appeared gives a leaf s of e that sent a value whole:
// the test's own call, the run's, another's, or an answer.
func classOf(e Earlier, s Seen) int {
	switch {
	case s.Side != Request:
		return 3
	case e.Own:
		return 0
	case e.Run:
		return 1
	}
	return 2
}

// firstsOf is key t's earliest leaves in m, made when there are none.
func (ix *servedIndex) firstsOf(m map[string]int32, t string) *[]firstsAt {
	id, ok := m[t]
	if !ok {
		id = int32(len(ix.firsts))
		ix.firsts = append(ix.firsts, nil)
		m[t] = id
	}
	return &ix.firsts[id]
}

// place adds leaf at, of the exchange last read, to a key's earliest leaves:
// of the same time as one read before, it comes after it.
func (ix *servedIndex) place(f *[]firstsAt, at leafAt) {
	cur := firstsAt{first: noLeaf, other: noLeaf}
	if len(*f) > 0 {
		cur = (*f)[len(*f)-1]
	}
	next := cur
	e := &ix.list[at.ex]
	switch {
	case cur.first == noLeaf:
		next.first = at
	case e.At.Before(ix.list[cur.first.ex].At):
		// The first is now at, and the first of another mock than at's the
		// one before it, or as it was when that one's mock is at's.
		if ix.list[cur.first.ex].Mock != e.Mock {
			next.other = cur.first
		}
		next.first = at
	case ix.list[cur.first.ex].Mock != e.Mock && (cur.other == noLeaf || e.At.Before(ix.list[cur.other.ex].At)):
		next.other = at
	default:
		return
	}
	next.from = at.ex
	if n := len(*f); n > 0 && (*f)[n-1].from == at.ex {
		(*f)[n-1] = next
		return
	}
	*f = append(*f, next)
}

// first is the earliest of a key's leaves in w, noLeaf when none.
//
// The leaves are read by when their exchanges were recorded first, so the
// earliest of w's mocks is the first or the other, and none of w's when that
// one was recorded after until: no later leaf was recorded before it.
func (w earlierView) first(f []firstsAt) leafAt {
	k := sort.Search(len(f), func(k int) bool { return f[k].from >= w.n }) - 1
	if k < 0 {
		return noLeaf
	}
	at := f[k].first
	if w.skip && w.ix.list[at.ex].Mock == w.mock {
		at = f[k].other
	}
	if at == noLeaf || w.ix.list[at.ex].At.After(w.until) {
		return noLeaf
	}
	return at
}

// before reports whether leaf a comes before b in what was served, as
// Classify reads it: by when its exchange was recorded, then in list order.
func (ix *servedIndex) before(a, b leafAt) bool {
	ta, tb := ix.list[a.ex].At, ix.list[b.ex].At
	switch {
	case ta.Before(tb):
		return true
	case tb.Before(ta):
		return false
	case a.ex != b.ex:
		return a.ex < b.ex
	}
	return a.leaf < b.leaf
}

// pair is drifted value d's entry, made and keyed by every rule of sources
// that can find it when it is new (ix.mu is held).
func (ix *servedIndex) pair(d *valueDrift, ex int32) *indexedPair {
	if id, ok := ix.pairOf[d]; ok {
		return ix.pairs[id]
	}
	id := int32(len(ix.pairs))
	p := &indexedPair{d: d, from: ex}
	ix.pairs = append(ix.pairs, p)
	ix.pairOf[d] = id
	key := func(m map[string][]int32, k string) {
		if l := m[k]; len(l) == 0 || l[len(l)-1] != id {
			m[k] = append(l, id)
		}
	}
	once := func(l *[]int32) {
		if len(*l) == 0 || (*l)[len(*l)-1] != id {
			*l = append(*l, id)
		}
	}
	key(ix.exact, d.rec)
	for w := range d.wholes {
		key(ix.wholes, w)
	}
	for i := range readings {
		for t := range d.lost[i] {
			key(ix.lost[i], t)
		}
		for t := range d.gained[i] {
			key(ix.gained[i], t)
		}
	}
	for _, s := range sides(d, true) {
		for w := range tokensBy(s, isWord) {
			key(ix.recWord, w)
		}
		windows(s, func(w string) { key(ix.recWin, w) })
	}
	for _, s := range sides(d, false) {
		for w := range tokensBy(s, isWord) {
			key(ix.liveWord, w)
		}
		windows(s, func(w string) { key(ix.liveWin, w) })
	}
	// A piece that drifted only with the clock is no part, whatever the
	// other value (sources).
	for _, pc := range d.pieces {
		if isClock(pc, d.clock) {
			continue
		}
		switch {
		case longestWord(pc.rec) != "":
			key(ix.pieceRec, longestWord(pc.rec))
		case windowKey(pc.rec) != "":
			key(ix.pieceRec, windowKey(pc.rec))
		case longestWord(pc.live) != "":
			key(ix.pieceLive, longestWord(pc.live))
		case windowKey(pc.live) != "":
			key(ix.pieceLive, windowKey(pc.live))
		default:
			once(&ix.pieceAny)
		}
	}
	if d.clock.whole {
		for _, s := range sides(d, true) {
			if w := longestWord(s); w != "" {
				key(ix.clockRec, w)
			} else {
				once(&ix.clockAny)
			}
		}
	}
	if d.encodedRec != "" {
		for w := range tokensBy(d.encodedRec, isWord) {
			key(ix.encWord, w)
		}
		once(&ix.encAll)
	}
	return p
}

// sides are d's recorded value (live: its live one), raw and, where it has
// escapes, decoded: where sources finds a piece of another value.
func sides(d *valueDrift, recorded bool) []string {
	switch {
	case recorded && d.decoded:
		return []string{d.rec, d.decRec}
	case recorded:
		return []string{d.rec}
	case d.decoded:
		return []string{d.live, d.decLive}
	}
	return []string{d.live}
}

// longestWord is s's longest ASCII word (the first of that length), "" when
// it has none.
func longestWord(s string) string {
	out := ""
	for _, w := range wordsBy(s, isWord) {
		if len(w) > len(out) {
			out = w
		}
	}
	return out
}

// maxWindow is the longest stretch of bytes beyond ASCII a value is found
// by: two characters of most scripts beyond Latin.
const maxWindow = 6

// windows calls f with every stretch of s of 1 to maxWindow bytes, all
// beyond ASCII.
func windows(s string, f func(string)) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j <= len(s) && j-i <= maxWindow && s[j-1] >= 0x80; j++ {
			f(s[i:j])
		}
	}
}

// windowKey is the first maxWindow bytes (or fewer) of s's longest stretch
// beyond ASCII, "" when it has none: one of its windows, and of every value
// that holds s.
func windowKey(s string) string {
	out := ""
	for _, r := range nonASCIIRuns(s) {
		if len(r) > len(out) {
			out = r
		}
	}
	if len(out) > maxWindow {
		out = out[:maxWindow]
	}
	return out
}

// candidates are the drifted values of w that can be where v came from
// (sources): every one that is, and some that are not. Each rule of sources
// holds only where a key the index keeps is shared, because a value that
// holds another as a token (ContainsToken, containsWholeClock) holds each of
// its ASCII words as a word of its own:
//
//   - the same recorded value whole (exact), or the same value whole in
//     another reading (wholes);
//   - a whole clock read inside the other's base64 text: each word of the
//     read is a word of that text (clockRec, encWord);
//   - a word both lost and a word both gained, in one reading: a value that
//     lost one of v's lost words, and gained one of its gained, in that
//     reading (lost, gained), but for a word that drifted with v's clock,
//     which no other clock makes a part (clockWord);
//   - a piece of v drifted in the other: each word of its recorded text is a
//     word of the other's recorded value, and of its live text of its live
//     value (recWord, liveWord), and its first bytes beyond ASCII
//     (windowKey) a stretch of them (recWin, liveWin): the fewest of any;
//     a piece with neither (a '-' that became a '_') is looked for in every
//     value;
//   - a piece of the other drifted in v: its longest word is a word of v, or
//     its first bytes beyond ASCII a stretch of v's (pieceRec, pieceLive), or
//     it has neither (pieceAny).
//
// w.ix.mu is held.
func (w earlierView) candidates(v *valueDrift) []int32 {
	ix := w.ix
	seen := map[int32]bool{}
	var out []int32
	add := func(ids []int32) {
		for _, id := range ids {
			if !seen[id] && ix.pairs[id].from < w.n {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	add(ix.exact[v.rec])
	for x := range v.wholes {
		add(ix.wholes[x])
	}
	if v.encodedRec != "" {
		for x := range tokensBy(v.encodedRec, isWord) {
			add(ix.clockRec[x])
		}
		add(ix.clockAny)
	}
	if v.clock.whole {
		for _, s := range sides(v, true) {
			if x := longestWord(s); x != "" {
				add(ix.encWord[x])
			} else {
				add(ix.encAll)
			}
		}
	}
	for i := range readings {
		lost := map[int32]bool{}
		for t := range v.lost[i] {
			if ids := ix.lost[i][t]; len(ids) > 0 && !clockWord(t, true, v, v.clock) {
				for _, id := range ids {
					lost[id] = true
				}
			}
		}
		if len(lost) == 0 {
			continue
		}
		for t := range v.gained[i] {
			if ids := ix.gained[i][t]; len(ids) > 0 && !clockWord(t, false, v, v.clock) {
				for _, id := range ids {
					if lost[id] {
						add([]int32{id})
					}
				}
			}
		}
	}
	for _, p := range v.pieces {
		if isClock(p, v.clock) {
			continue
		}
		var fewest []int32
		keyed := false
		for x := range tokensBy(p.rec, isWord) {
			if l := ix.recWord[x]; !keyed || len(l) < len(fewest) {
				fewest, keyed = l, true
			}
		}
		for x := range tokensBy(p.live, isWord) {
			if l := ix.liveWord[x]; !keyed || len(l) < len(fewest) {
				fewest, keyed = l, true
			}
		}
		if x := windowKey(p.rec); x != "" {
			if l := ix.recWin[x]; !keyed || len(l) < len(fewest) {
				fewest, keyed = l, true
			}
		}
		if x := windowKey(p.live); x != "" {
			if l := ix.liveWin[x]; !keyed || len(l) < len(fewest) {
				fewest, keyed = l, true
			}
		}
		if keyed {
			add(fewest)
			continue
		}
		for id, e := range ix.pairs {
			if codrifted(p, e.d.rec, e.d.live) || e.d.decoded && codrifted(p, e.d.decRec, e.d.decLive) {
				add([]int32{int32(id)})
			}
		}
	}
	for _, s := range sides(v, true) {
		for x := range tokensBy(s, isWord) {
			add(ix.pieceRec[x])
		}
		windows(s, func(x string) { add(ix.pieceRec[x]) })
	}
	for _, s := range sides(v, false) {
		for x := range tokensBy(s, isWord) {
			add(ix.pieceLive[x])
		}
		windows(s, func(x string) { add(ix.pieceLive[x]) })
	}
	add(ix.pieceAny)
	return out
}

// source is the earlier place of w that drifted value v came from
// (appeared): of the leaves sources finds, the one of the best rank, the
// earliest within it. A leaf's rank is known before sources reads it (it
// finds a value whole where, and only where, the recorded value is v's), so
// the candidates' leaves are read best first, and the first it finds is the
// place.
func (w earlierView) source(v *valueDrift) (Place, bool) {
	ix := w.ix
	ix.mu.Lock()
	defer ix.mu.Unlock()
	type leaf struct {
		rank int
		at   leafAt
		id   int32
	}
	var leaves []leaf
	for _, id := range w.candidates(v) {
		p := ix.pairs[id]
		for c := range p.class {
			at := w.first(p.class[c])
			if at == noLeaf {
				continue
			}
			r := 6
			if c < 3 {
				r = c
				if p.d.rec != v.rec {
					r += 3
				}
			}
			leaves = append(leaves, leaf{r, at, id})
		}
	}
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].rank != leaves[j].rank {
			return leaves[i].rank < leaves[j].rank
		}
		return ix.before(leaves[i].at, leaves[j].at)
	})
	read := map[int32]bool{}
	for _, l := range leaves {
		found, ok := read[l.id]
		if !ok {
			found, _ = sources(ix.pairs[l.id].d, v)
			read[l.id] = found
		}
		if !found {
			continue
		}
		e := &ix.list[l.at.ex]
		s := e.Seen[l.at.leaf]
		return Place{Mock: e.Mock, Side: s.Side, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
			Var: s.Place, Live: s.Live, Whole: ix.pairs[l.id].d.rec == v.rec, Own: e.Own, Run: e.Run, Name: s.Name}, true
	}
	return Place{}, false
}

// holding is the mock and the path of the earliest leaf of w that holds
// word t in reading i, in its recorded value (recorded) or its live one, and
// whether its exchange is the run's (Earlier.Run); ok is false when none
// does.
func (w earlierView) holding(i int, t string, recorded bool) (mock, path string, run, ok bool) {
	ix := w.ix
	ix.mu.Lock()
	defer ix.mu.Unlock()
	m := ix.liveTok[i]
	if recorded {
		m = ix.recTok[i]
	}
	id, ok := m[t]
	if !ok {
		return "", "", false, false
	}
	at := w.first(ix.firsts[id])
	if at == noLeaf {
		return "", "", false, false
	}
	e := &ix.list[at.ex]
	return e.Mock, e.Seen[at.leaf].Leaf.Path, e.Run, true
}

// rawRead is a mock's payload as the learn step reads it, once per test
// (DriftCache.RawOf): once read (words), its words in every declared form,
// by reading (rawTokens), and what it is searched in besides itself
// (rawExtra).
type rawRead struct {
	text  string
	once  sync.Once
	toks  []map[string]bool
	extra string
}

// words reads r's words and its other forms, once.
func (r *rawRead) words() {
	r.once.Do(func() {
		forms := textForms(r.text)
		r.toks = formTokens(forms)
		r.extra = strings.Join(forms[1:], "\n")
	})
}

// rawView is a request's payloads (Raw) read in the test's cache: raw as
// given, and of each payload the first place in raw where it is an earlier
// place (served, or recorded before at). pos is nil when a payload of raw
// was not read by the cache (RawOf): raw is read in full, as given.
type rawView struct {
	raw []Raw
	at  time.Time
	pos map[*rawRead]int
}

// rawView reads raw, each payload once for the test.
func (dc *DriftCache) rawView(raw []Raw, at time.Time) rawView {
	v := rawView{raw: raw, at: at}
	for _, x := range raw {
		if x.read == nil {
			return v
		}
	}
	v.pos = make(map[*rawRead]int, len(raw))
	for i, x := range raw {
		if !x.Served && !x.At.Before(at) {
			continue
		}
		if _, ok := v.pos[x.read]; ok {
			continue
		}
		v.pos[x.read] = i
		x.read.words()
		dc.rawMu.Lock()
		if !dc.rawIndexed[x.read] {
			dc.rawIndexed[x.read] = true
			for r, toks := range x.read.toks {
				for t := range toks {
					dc.rawHolders[r][t] = append(dc.rawHolders[r][t], x.read)
				}
			}
		}
		dc.rawMu.Unlock()
	}
	return v
}

// first is the first place in v's raw of an earlier place whose words in
// reading i hold t, -1 when none (v.pos is not nil).
func (v rawView) first(dc *DriftCache, i int, t string) int {
	dc.rawMu.Lock()
	hs := dc.rawHolders[i][t]
	dc.rawMu.Unlock()
	best := -1
	for _, r := range hs {
		if p, ok := v.pos[r]; ok && (best < 0 || p < best) {
			best = p
		}
	}
	return best
}

// holders are the places in v's raw, in order, of the earlier places that
// hold the longest ASCII word of each of keys; ok is false when one has
// none, or v.pos is nil (and every payload is read).
func (v rawView) holders(dc *DriftCache, keys []string) ([]int, bool) {
	if v.pos == nil {
		return nil, false
	}
	seen := map[int]bool{}
	var out []int
	for _, k := range keys {
		w := longestWord(k)
		if w == "" {
			return nil, false
		}
		dc.rawMu.Lock()
		hs := dc.rawHolders[0][w]
		dc.rawMu.Unlock()
		for _, r := range hs {
			if p, ok := v.pos[r]; ok && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Ints(out)
	return out, true
}

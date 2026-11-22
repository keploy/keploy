package manager

import (
	"math"
	"slices"
	"time"
)

// windowIndex finds the windows of the ring (SyncMockManager.recentWindows)
// that may hold an instant, or reach into a span, without looking at the
// others: a search costs O((k+1) log W) for the k windows it finds among the
// W the ring keeps, where a walk of the ring cost O(W). Every pass over the
// buffer asks it once for each mock it decides (ownerOrSharedLocked), so a
// pass over a large buffer with a full ring no longer holds mu for buffer ×
// ring steps.
//
// It is a treap (a binary search tree kept balanced by random priorities)
// ordered by each window's start, then by its place in the ring, and each node
// keeps the latest end in its subtree: a search skips every subtree whose
// windows all end before the span it is asked about, and every window that
// starts after it.
//
// It keys a time by its wall clock (wallKey), while time.Time compares two
// times by their monotonic readings when both have one. The two clocks agree
// on the order of two times unless the wall clock was stepped between them,
// so a search widens by how far the offsets between the two clocks spread
// over the times it holds (skew), and its callers check what it finds with
// the times' own comparisons. So it finds every window a walk of the ring
// would, and a few more at most; its callers keep the walk's order where the
// order decides (the earliest window in the ring wins).
//
// After a wall-clock step (a laptop's suspend is one too: Go's monotonic clock
// does not count the time suspended on Linux), every search is widened by the
// step until the skew is worked out again with no window from before the step
// left in the ring: up to two turns of the ring, 2 × maxRecentWindows
// resolves, since the ring lets windows go by count, not by age. Meanwhile a
// search finds every window within the step of the instant it is asked
// about, so after a step of minutes or more an owner lookup costs up to the
// walk's O(W) again, until then. The answers stay the walk's, and the settled
// prefix (settledPrefix) still bounds how many mocks a pass looks up.
//
// The ring's windows carry their place in it (resolvedWindow.seq). The index
// is in step with the ring while it holds as many windows as the ring, the
// first and the last at the places it holds at its ends. resolveRange is the
// only writer of the ring, and keeps it so; a row that adds to the ring or
// cuts it other than through resolveRange makes the index rebuild
// (windowsLocked). A write that keeps the ring's length and both its ends is
// not seen: no code makes one.
//
// Memory: 48 bytes a window beside the ring's own 96, so about 1.2 MB for a
// full ring. Guarded by mu.
type windowIndex struct {
	// nodes[0] is unused: id 0 is no node, so the zero index is empty.
	nodes []winNode
	free  []int32
	root  int32
	// first is the place of the ring's first window, next the place the
	// next window resolved gets, and n how many the index holds.
	first, next uint64
	n           int
	// gen counts the rebuilds: what a pass learnt of the buffer against the
	// ring (settledPrefix) holds only while the ring was not rebuilt since.
	gen uint64
	// rng makes the priorities, from a fixed seed: a ring has the same shape
	// in every run.
	rng uint32
	// skew is the spread of the clock offsets (clockOffset) of the windows'
	// starts and ends that have a monotonic reading: of every window that was
	// in the ring when it was last worked out, and of every one added since.
	// It is worked out again from the ring once those of the first kind have
	// all left (skewUntil, reskew), so it narrows again after a clock step.
	skew      skewSpan
	skewUntil uint64
	// found is the places the last search found, reused so a search
	// allocates nothing.
	found []int32
	// visited counts the nodes the searches looked at: the work the rows
	// that bound a pass's cost check.
	visited uint64
}

// winNode is a window of the ring in the index: its start and end (wallKey),
// the latest end in its subtree, and its place in the ring.
type winNode struct {
	start, end, maxEnd int64
	seq                uint64
	left, right        int32
	prio               uint32
}

// noNode is no node.
const noNode = 0

// skewSpan is the least and the greatest of some clock offsets; ok says there
// are any.
type skewSpan struct {
	lo, hi int64
	ok     bool
}

func (s *skewSpan) add(off int64) {
	if !s.ok {
		s.lo, s.hi, s.ok = off, off, true
		return
	}
	s.lo = min(s.lo, off)
	s.hi = max(s.hi, off)
}

// addTimes adds the clock offsets of w's start and end that have one.
func (s *skewSpan) addTimes(w *resolvedWindow) {
	if off, ok := clockOffset(w.start); ok {
		s.add(off)
	}
	if off, ok := clockOffset(w.end); ok {
		s.add(off)
	}
}

var (
	wallKeyMin = time.Unix(0, math.MinInt64)
	wallKeyMax = time.Unix(0, math.MaxInt64)
	// clockBase is the time the clock offsets are measured from. It has a
	// monotonic reading wherever the platform gives one (clockBaseMono).
	clockBase     = time.Now()
	clockBaseKey  = clockBase.UnixNano()
	clockBaseMono = hasMonotonic(clockBase)
)

// wallKey is t's wall clock in Unix nanoseconds, or the bound of an int64
// that t is past (a time before 1678 or after 2262, the zero time among
// them). Of two times compared by their wall clocks, the earlier never has
// the greater key.
func wallKey(t time.Time) int64 {
	switch {
	case t.Before(wallKeyMin):
		return math.MinInt64
	case t.After(wallKeyMax):
		return math.MaxInt64
	}
	return t.UnixNano()
}

// hasMonotonic reports whether t carries a monotonic clock reading.
func hasMonotonic(t time.Time) bool {
	return t != t.Round(0)
}

// clockOffset is how far t's wall clock is from its monotonic reading,
// measured against clockBase's, when t has a monotonic reading. Two times that
// both have one compare as their wall clocks less their offsets do: a is
// before b exactly when wallKey(a)-off(a) < wallKey(b)-off(b). A time with a
// monotonic reading lies between 1885 and 2157, so neither difference
// overflows.
func clockOffset(t time.Time) (int64, bool) {
	if !hasMonotonic(t) {
		return 0, false
	}
	return (t.UnixNano() - clockBaseKey) - int64(t.Sub(clockBase)), true
}

// satAdd and satSub are a+b and a-b for b >= 0, held at the int64 bounds.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func satSub(a, b int64) int64 {
	if a < math.MinInt64+b {
		return math.MinInt64
	}
	return a - b
}

// windowsLocked is the ring's index, rebuilt first if the ring was changed
// other than by resolveRange. Caller holds mu.
func (m *SyncMockManager) windowsLocked() *windowIndex {
	ix := &m.windows
	if !ix.inStep(m.recentWindows) {
		ix.rebuild(m.recentWindows)
	}
	return ix
}

// inStep reports whether the index holds ring: as many windows, the first and
// the last of them at the places it holds at its ends.
func (ix *windowIndex) inStep(ring []resolvedWindow) bool {
	n := len(ring)
	if n != ix.n {
		return false
	}
	return n == 0 || (ring[0].seq == ix.first && ring[n-1].seq == ix.first+uint64(n-1))
}

// rebuild makes the index hold ring, numbering its windows on from the
// places given out so far.
func (ix *windowIndex) rebuild(ring []resolvedWindow) {
	if len(ix.nodes) > 0 {
		ix.nodes = ix.nodes[:1]
	}
	ix.free, ix.root, ix.n = ix.free[:0], noNode, 0
	ix.first = ix.nextSeq()
	ix.skew = skewSpan{}
	for i := range ring {
		ring[i].seq = ix.nextSeq()
		ix.add(ring[i])
	}
	ix.skewUntil = ix.next
	ix.gen++
}

// nextSeq is the place the next window added to the ring takes. 0 is no
// place: a window a row put in the ring has it.
func (ix *windowIndex) nextSeq() uint64 {
	if ix.next == 0 {
		ix.next = 1
	}
	return ix.next
}

// add puts w, the ring's newest window, in the index. w.seq is nextSeq().
func (ix *windowIndex) add(w resolvedWindow) {
	if ix.n == 0 {
		ix.first = w.seq
	}
	ix.next = w.seq + 1
	ix.n++
	nd := winNode{start: wallKey(w.start), end: wallKey(w.end), seq: w.seq, prio: ix.rand()}
	nd.maxEnd = nd.end
	ix.skew.addTimes(&w)
	id := ix.alloc(nd)
	l, r := ix.split(ix.root, nd.start, nd.seq)
	ix.root = ix.merge(ix.merge(l, id), r)
}

// alloc stores nd and returns its id.
func (ix *windowIndex) alloc(nd winNode) int32 {
	if k := len(ix.free); k > 0 {
		id := ix.free[k-1]
		ix.free = ix.free[:k-1]
		ix.nodes[id] = nd
		return id
	}
	if len(ix.nodes) == 0 {
		ix.nodes = append(ix.nodes, winNode{}) // id 0: no node
	}
	ix.nodes = append(ix.nodes, nd)
	return int32(len(ix.nodes) - 1)
}

// removeFirst takes w, the ring's oldest window, out of the index. Should it
// not be there, the index is put out of step, and the next windowsLocked
// rebuilds it from the ring.
func (ix *windowIndex) removeFirst(w *resolvedWindow) {
	start := wallKey(w.start)
	l, r := ix.split(ix.root, start, w.seq)
	mid, r := ix.split(r, start, w.seq+1)
	ix.root = ix.merge(l, r)
	if mid == noNode || ix.nodes[mid].seq != w.seq || ix.nodes[mid].left != noNode || ix.nodes[mid].right != noNode || w.seq != ix.first {
		ix.n = -1
		return
	}
	ix.free = append(ix.free, mid)
	ix.n--
	ix.first = w.seq + 1
}

// reskew works the skew out again from ring, the windows the index holds,
// once every window that was in the ring when it was last worked out has
// left: one pass of the ring for each turn of it.
func (ix *windowIndex) reskew(ring []resolvedWindow) {
	if ix.first < ix.skewUntil {
		return
	}
	ix.skew = skewSpan{}
	ix.skewUntil = ix.next
	for i := range ring {
		ix.skew.addTimes(&ring[i])
	}
}

// rand is the next priority, from a xorshift generator.
func (ix *windowIndex) rand() uint32 {
	if ix.rng == 0 {
		ix.rng = 0x9e3779b9
	}
	x := ix.rng
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	ix.rng = x
	return x
}

// before reports whether nd sorts before the key (start, seq).
func (nd *winNode) before(start int64, seq uint64) bool {
	return nd.start < start || (nd.start == start && nd.seq < seq)
}

// split splits the subtree t into its nodes that sort before (start, seq) and
// the rest.
func (ix *windowIndex) split(t int32, start int64, seq uint64) (int32, int32) {
	if t == noNode {
		return noNode, noNode
	}
	if ix.nodes[t].before(start, seq) {
		l, r := ix.split(ix.nodes[t].right, start, seq)
		ix.nodes[t].right = l
		ix.pull(t)
		return t, r
	}
	l, r := ix.split(ix.nodes[t].left, start, seq)
	ix.nodes[t].left = r
	ix.pull(t)
	return l, t
}

// merge joins the subtrees a and b, every node of a sorting before every node
// of b.
func (ix *windowIndex) merge(a, b int32) int32 {
	if a == noNode {
		return b
	}
	if b == noNode {
		return a
	}
	if ix.nodes[a].prio > ix.nodes[b].prio {
		ix.nodes[a].right = ix.merge(ix.nodes[a].right, b)
		ix.pull(a)
		return a
	}
	ix.nodes[b].left = ix.merge(a, ix.nodes[b].left)
	ix.pull(b)
	return b
}

// pull sets t's latest end from its own end and its children's.
func (ix *windowIndex) pull(t int32) {
	nd := &ix.nodes[t]
	end := nd.end
	if nd.left != noNode {
		end = max(end, ix.nodes[nd.left].maxEnd)
	}
	if nd.right != noNode {
		end = max(end, ix.nodes[nd.right].maxEnd)
	}
	nd.maxEnd = end
}

// reaching returns the places in the ring, in no order, of windows among
// which is every window w that reaches into [from, to] as the times compare:
// !w.start.After(to) && !w.end.Before(from). There may be a few more, as close
// to the span as the clocks' skew: the caller checks each. The slice is the
// index's, good until its next search.
func (ix *windowIndex) reaching(from, to time.Time) []int32 {
	ix.found = ix.found[:0]
	if ix.root == noNode {
		return ix.found
	}
	lo, hi := wallKey(from), wallKey(to)
	switch {
	case !clockBaseMono:
		// No offset can be worked out, so no wall clock narrows a search.
		lo, hi = math.MinInt64, math.MaxInt64
	case ix.skew.ok:
		fromOff, fromMono := clockOffset(from)
		toOff, toMono := fromOff, fromMono
		if to != from {
			toOff, toMono = clockOffset(to)
		}
		lo, hi = ix.widen(lo, hi, fromOff, fromMono, toOff, toMono)
	}
	ix.collect(ix.root, lo, hi)
	return ix.found
}

// widen widens the keys [lo, hi] of a search from `from` to `to` (their clock
// offsets, if they have a monotonic reading) by the skew. A window's start
// and to compare by their monotonic readings when both have one: the start
// may then be after to's wall clock by as much as its offset is above to's.
// Its end and from likewise: the end may be before from's wall clock by as
// much as its offset is below from's.
func (ix *windowIndex) widen(lo, hi, fromOff int64, fromMono bool, toOff int64, toMono bool) (int64, int64) {
	if toMono && ix.skew.hi > toOff {
		hi = satAdd(hi, ix.skew.hi-toOff)
	}
	if fromMono && fromOff > ix.skew.lo {
		lo = satSub(lo, fromOff-ix.skew.lo)
	}
	return lo, hi
}

// holding is reaching(t, t): windows among which is every one that holds t.
func (ix *windowIndex) holding(t time.Time) []int32 {
	return ix.reaching(t, t)
}

// collect adds to found the places of the nodes of the subtree t that start
// at or before hi and end at or after lo.
func (ix *windowIndex) collect(t int32, lo, hi int64) {
	for t != noNode {
		nd := &ix.nodes[t]
		ix.visited++
		if nd.maxEnd < lo {
			return
		}
		ix.collect(nd.left, lo, hi)
		if nd.start > hi {
			return
		}
		if nd.end >= lo {
			ix.found = append(ix.found, int32(nd.seq-ix.first))
		}
		t = nd.right
	}
}

// inRingOrder sorts places a search found into the ring's order.
func inRingOrder(found []int32) []int32 {
	slices.Sort(found)
	return found
}

// addRecentWindowLocked adds w to the ring as its newest window, and lets go
// of the oldest past maxRecentWindows. The ring moves on past the windows it
// lets go of (cleared, so their names are not kept) rather than copying the
// rest down over them: append copies the ring into a new array only once the
// old one is used up, so a resolve does not copy the ring. Caller holds mu.
func (m *SyncMockManager) addRecentWindowLocked(w resolvedWindow) {
	ix := m.windowsLocked()
	w.seq = ix.nextSeq()
	m.recentWindows = append(m.recentWindows, w)
	ix.add(w)
	if over := len(m.recentWindows) - maxRecentWindows; over > 0 {
		for i := range over {
			ix.removeFirst(&m.recentWindows[i])
		}
		clear(m.recentWindows[:over])
		m.recentWindows = m.recentWindows[over:]
		ix.reskew(m.recentWindows)
	}
}

// passWorkLocked is the work the passes over the buffer did so far: the
// buffered mocks they decided, and the index's nodes their lookups looked at.
// For the rows that bound a pass's work. Caller holds mu.
func (m *SyncMockManager) passWorkLocked() (decided, visited uint64) {
	return m.decided, m.windows.visited
}

package models

import (
	"slices"
	"strings"
)

// IDKeyMinDigits is the fewest digits a key of digits alone holds to be an
// id (matcher.IsIDKey reads it so): ten digits is the shortest epoch second
// of this century, and past any count, year, HTTP status, YYYYMMDD date or
// small serial id.
const IDKeyMinDigits = 10

// shapedRuns is text with each run of whole fields of paths (field paths
// FieldShape changes) in its shape, where the text names it as whole
// fields: beginning at the text's start, after a "." or after a mark that
// ends a word (wordEnd), and ending at the text's end, at a ".", at an
// index ("[") or at such a mark. A run is any of a path's fields in a row,
// the last also without its indexes ("tags" of "tags[0]"), as a hint names
// the keploy.yml entry that matched a field. The longest run at a place
// wins, and a run FieldShape keeps (no field of it may be a value) is left.
// So is a count alone, a number shorter than an id (IDKeyMinDigits): a
// text's counts are numbers ("and 3 more reasons"), and a path's position
// ("docs.0.q") is shaped where the text names it with the fields beside it.
// A number as long as an id is shaped alone too.
//
// No run is ever built: the paths' fields are read once into a suffix
// automaton (runSet), and the text is read once, field by field
// (runScan), so the cost is linear in the text and the paths however deep
// a path is. (Every run of every path was once built and shaped: a second
// and 650 MB for each miss whose paths are 120 fields deep.)
func shapedRuns(text string, paths []string) string {
	r := newRunSet(paths)
	if r == nil {
		return text
	}
	sc := newRunScan(r, text)
	var b strings.Builder
	done := 0
	for i := 0; i < len(text); {
		if i == 0 || text[i-1] == '.' || wordEnd(text[i-1]) {
			if end := sc.longest(i); end > i {
				if done == 0 {
					b.Grow(len(text))
				}
				b.WriteString(text[done:i])
				b.WriteString(FieldShape(text[i:end]))
				done, i = end, end
				continue
			}
		}
		i++
	}
	if done == 0 {
		return text
	}
	b.WriteString(text[done:])
	return b.String()
}

// runSet is the runs of paths' fields: each field (and each field's name
// before its indexes, its base) by an id, and a suffix automaton over the
// paths' fields, each path ended by a mark of its own so no run crosses two
// paths. Read from state 0 field by field, a run's fields end in a state;
// any other sequence of fields falls out (no transition).
type runSet struct {
	ids    map[string]int32 // a field or a base, by its id
	shaped []bool           // by id: FieldShape changes it
	maxLen int              // the longest field or base

	next    map[uint64]int32 // (state, field id) -> state
	base    map[uint64]bool  // (state, base id): a field leaving the state has that base
	link    []int32          // by state: its suffix link
	length  []int32          // by state: the longest run (in fields) it holds
	head    []int32          // by state: its last edge, -1 for none
	edgeSym []int32          // by edge: its field id (a path's end mark is < 0)
	edgeNxt []int32          // by edge: the state's edge before it, -1 for none
}

func runKey(state, sym int32) uint64 { return uint64(uint32(state))<<32 | uint64(uint32(sym)) }

// newRunSet is the runs of paths, nil when there are none. A path that is
// another's without its "body." adds no run: its runs are the other's.
func newRunSet(paths []string) *runSet {
	r := &runSet{ids: map[string]int32{}}
	var baseOf []int32
	intern := func(s string) int32 {
		if id, ok := r.ids[s]; ok {
			return id
		}
		id := int32(len(r.shaped))
		r.ids[s] = id
		r.shaped = append(r.shaped, FieldShape(s) != s)
		baseOf = append(baseOf, -1)
		r.maxLen = max(r.maxLen, len(s))
		return id
	}
	kept, n := make([]string, 0, len(paths)), 0
	for _, p := range paths {
		if !slices.Contains(paths, "body."+p) {
			kept = append(kept, p)
			n += strings.Count(p, ".") + 2
		}
	}
	// A suffix automaton of n marks has at most 2n states and 3n edges.
	r.next = make(map[uint64]int32, 2*n)
	r.length, r.link, r.head = make([]int32, 0, 2*n), make([]int32, 0, 2*n), make([]int32, 0, 2*n)
	r.edgeSym, r.edgeNxt = make([]int32, 0, 2*n), make([]int32, 0, 2*n)
	r.addState(0)
	r.link[0] = -1
	mark := int32(-1)
	for _, p := range kept {
		last := int32(0)
		for _, f := range strings.Split(p, ".") {
			id := intern(f)
			if k := strings.IndexByte(f, '['); k > 0 {
				b := intern(f[:k])
				baseOf[id] = b
			}
			last = r.extend(last, id)
		}
		r.extend(last, mark)
		mark--
	}
	if len(r.shaped) == 0 {
		return nil
	}
	for s := range r.head {
		for e := r.head[s]; e >= 0; e = r.edgeNxt[e] {
			if sym := r.edgeSym[e]; sym >= 0 && baseOf[sym] >= 0 {
				if r.base == nil {
					r.base = map[uint64]bool{}
				}
				r.base[runKey(int32(s), baseOf[sym])] = true
			}
		}
	}
	return r
}

func (r *runSet) addState(length int32) int32 {
	r.length = append(r.length, length)
	r.link = append(r.link, 0)
	r.head = append(r.head, -1)
	return int32(len(r.length) - 1)
}

func (r *runSet) addEdge(from, sym, to int32) {
	r.next[runKey(from, sym)] = to
	r.edgeSym = append(r.edgeSym, sym)
	r.edgeNxt = append(r.edgeNxt, r.head[from])
	r.head[from] = int32(len(r.edgeSym) - 1)
}

// extend adds sym after the state last (the whole sequence read so far) and
// returns the state of the longer sequence: the suffix automaton's online
// construction.
func (r *runSet) extend(last, sym int32) int32 {
	cur := r.addState(r.length[last] + 1)
	p := last
	for p >= 0 {
		if _, ok := r.next[runKey(p, sym)]; ok {
			break
		}
		r.addEdge(p, sym, cur)
		p = r.link[p]
	}
	if p < 0 {
		return cur // link 0
	}
	q := r.next[runKey(p, sym)]
	if r.length[p]+1 == r.length[q] {
		r.link[cur] = q
		return cur
	}
	clone := r.addState(r.length[p] + 1)
	for e := r.head[q]; e >= 0; e = r.edgeNxt[e] {
		s := r.edgeSym[e]
		r.addEdge(clone, s, r.next[runKey(q, s)])
	}
	r.link[clone] = r.link[q]
	// No transition leads back to state 0, so a missing one reads 0, never q.
	for p >= 0 && r.next[runKey(p, sym)] == q {
		r.next[runKey(p, sym)] = clone
		p = r.link[p]
	}
	r.link[q] = clone
	r.link[cur] = clone
	return cur
}

// follows reports whether the field or base id may end a run read to state:
// a run's fields go on with that field, or with a field of that base.
func (r *runSet) follows(state, id int32) bool {
	if _, ok := r.next[runKey(state, id)]; ok {
		return true
	}
	return r.base[runKey(state, id)]
}

// runScan reads a text's runs of a runSet's paths. The text's segments are
// its parts between its "."s: a run's first field is the rest of a segment
// from where the run begins, the fields after it are whole segments, and its
// last field is a segment's start up to a field's end ("tags" of "tags[] and
// more"), or a whole one.
type runScan struct {
	r    *runSet
	text string
	dots []int32 // where the text's "."s are
	seg  []int32 // by segment: the id of the whole segment, -1 for none
	// By segment s: how many segments before s are a field FieldShape
	// changes.
	shapedSegs []int32
	si         int // the segment the scan is in

	// Runs of whole segments from a segment's start, read as a window that
	// only moves on: segments [wb, we] are the longest run from wb, read to
	// the state wst.
	wb, we int
	wst    int32
}

func newRunScan(r *runSet, text string) *runScan {
	sc := &runScan{r: r, text: text}
	for i := 0; i < len(text); i++ {
		if text[i] == '.' {
			sc.dots = append(sc.dots, int32(i))
		}
	}
	n := len(sc.dots) + 1
	sc.seg = make([]int32, n)
	sc.shapedSegs = make([]int32, n+1)
	for s := range n {
		sc.seg[s] = sc.id(sc.segStart(s), sc.segEnd(s))
		sc.shapedSegs[s+1] = sc.shapedSegs[s] + b2i(sc.seg[s] >= 0 && r.shaped[sc.seg[s]])
	}
	sc.wb, sc.we = 0, -1
	return sc
}

func b2i(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

func (sc *runScan) segStart(s int) int {
	if s == 0 {
		return 0
	}
	return int(sc.dots[s-1]) + 1
}

func (sc *runScan) segEnd(s int) int {
	if s < len(sc.dots) {
		return int(sc.dots[s])
	}
	return len(sc.text)
}

// id is the id of text[from:to], -1 when it is no field or base.
func (sc *runScan) id(from, to int) int32 {
	if to-from > sc.r.maxLen {
		return -1
	}
	if id, ok := sc.r.ids[sc.text[from:to]]; ok {
		return id
	}
	return -1
}

// partID is the id of text[from:j], a field ending inside its segment
// (which ends at end): -1 when j is no field's end ("[" or a mark that ends
// a word) or the text is no field or base.
func (sc *runScan) partID(from, j, end int) int32 {
	if j >= end || (sc.text[j] != '[' && !wordEnd(sc.text[j])) {
		return -1
	}
	return sc.id(from, j)
}

// shapedIn reports whether a whole segment of [a, b] is a field FieldShape
// changes.
func (sc *runScan) shapedIn(a, b int) bool { return sc.shapedSegs[b+1] > sc.shapedSegs[a] }

// window is the longest run of whole segments from segment b (how many, 0
// for none) and its state. b never goes back.
func (sc *runScan) window(b int) (int, int32) {
	r := sc.r
	if b > sc.we {
		sc.wb, sc.we, sc.wst = b, b-1, 0
	}
	for ; sc.wb < b; sc.wb++ {
		// The run without its first field: its state, or that state's
		// suffix link when the shorter run is the link's.
		if l := int32(sc.we - sc.wb); l == 0 {
			sc.wst = 0
		} else if r.length[r.link[sc.wst]] >= l {
			sc.wst = r.link[sc.wst]
		}
	}
	for sc.we+1 < len(sc.seg) {
		id := sc.seg[sc.we+1]
		if id < 0 {
			break
		}
		nx, ok := r.next[runKey(sc.wst, id)]
		if !ok {
			break
		}
		sc.wst, sc.we = nx, sc.we+1
	}
	return sc.we - sc.wb + 1, sc.wst
}

// valid reports whether a run of n fields, text[i:j], is one to shape: some
// field of it FieldShape changes (shaped, for n of 3 or more; a and b are
// the ids of the first two fields), and it is no count alone. FieldShape
// reads two fields as one name in two cases: a header's name
// ("header.X-Trace") and an SQL place (bind.$1, literal.3).
func (sc *runScan) valid(i, j, n int, a, b int32, shaped bool) bool {
	run := sc.text[i:j]
	switch n {
	case 1:
		return sc.r.shaped[a] && !countAlone(run)
	case 2:
		if f, _, _ := strings.Cut(run, "."); f == "header" || f == "bind" || f == "literal" {
			return FieldShape(run) != run
		}
		return sc.r.shaped[a] || sc.r.shaped[b]
	}
	return shaped
}

// countAlone reports whether run is a number shorter than an id: a count a
// text says ("and 3 more reasons"), never shaped alone.
func countAlone(run string) bool {
	return len(run) < IDKeyMinDigits && strings.Trim(run, "0123456789") == ""
}

// longest is the end of the longest run to shape that begins at i (a
// place a run may begin), or 0 for none.
func (sc *runScan) longest(i int) int {
	r := sc.r
	for sc.si < len(sc.dots) && int(sc.dots[sc.si]) < i {
		sc.si++
	}
	si := sc.si
	start, end := sc.segStart(si), sc.segEnd(si)

	// The chain: the rest of the segment, then whole segments, read as far
	// as they are a run (n fields, to state st; shaped: one of them is a
	// field FieldShape changes).
	id0 := sc.id(i, end)
	var n int
	var st int32
	var shaped bool
	if i == start {
		n, st = sc.window(si)
		shaped = n > 0 && sc.shapedIn(si, si+n-1)
	} else if nx, ok := r.next[runKey(0, id0)]; ok && id0 >= 0 {
		n, st, shaped = 1, nx, r.shaped[id0]
		for s := si + 1; s < len(sc.seg) && sc.seg[s] >= 0; s++ {
			nx, ok := r.next[runKey(st, sc.seg[s])]
			if !ok {
				break
			}
			n, st, shaped = n+1, nx, shaped || r.shaped[sc.seg[s]]
		}
	}
	// segID is the id of the chain's k-th field.
	segID := func(k int) int32 {
		if k == 0 {
			return id0
		}
		return sc.seg[si+k]
	}
	// lastPart is the end of the longest run of the chain's first k fields
	// (read to state at) and the start of segment s up to a field's end
	// (whole too, as a base, or empty), or 0.
	lastPart := func(k int, at int32, s int, kShaped bool) int {
		from, to := sc.segStart(s), sc.segEnd(s)
		// The last field may be empty ("7." before a mark that ends a
		// word, of a path ending "7.").
		for j := min(to, from+r.maxLen); j >= from; j-- {
			var id int32
			if j == to {
				id = sc.id(from, j)
			} else {
				id = sc.partID(from, j, to)
			}
			if id < 0 || !r.follows(at, id) {
				continue
			}
			// Two fields are the first and this one; more are read by
			// kShaped.
			if sc.valid(i, j, k+1, segID(0), id, kShaped || r.shaped[id]) {
				return j
			}
		}
		return 0
	}
	if n > 0 {
		last := si + n - 1
		// The whole chain and a field's start of the segment after it.
		if last+1 < len(sc.seg) {
			if j := lastPart(n, st, last+1, shaped); j > 0 {
				return j
			}
		}
		var second int32 = -1
		if n > 1 {
			second = segID(1)
		}
		if sc.valid(i, sc.segEnd(last), n, id0, second, shaped) {
			return sc.segEnd(last)
		}
		// Shorter. When the whole chain is no run to shape, none of its
		// fields is shaped, but for a header's name that is no name read
		// as two fields: "header.a:b.c" is no run to shape, "header.a:b"
		// is ("header.{}"). A field FieldShape keeps is names, separators
		// and indexes, and so is any start of it up to a field's end, so
		// no other shorter run beginning here is one to shape.
		if n >= 2 {
			if j := lastPart(1, r.next[runKey(0, id0)], si+1, false); j > 0 {
				return j
			}
		}
	}
	// One field ending inside the segment, or the rest of it as a base.
	for j := min(end, i+r.maxLen); j > i; j-- {
		var id int32
		switch {
		case j < end:
			id = sc.partID(i, j, end)
		case n == 0:
			id = id0
		default:
			continue
		}
		if id >= 0 && r.follows(0, id) && sc.valid(i, j, 1, id, -1, false) {
			return j
		}
	}
	return 0
}

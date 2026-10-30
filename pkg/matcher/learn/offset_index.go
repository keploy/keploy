package learn

import (
	"sort"
	"sync"
	"time"
)

// offsetIndex is the places the offset rule reads (offsetPlacesOf) of a
// set's recordings, each recording's read once for the set (keyedPlaces,
// GivenPlaces) and indexed by place (offsetKey, in keyLess's order) and,
// within one, by moment: the handlers of the set share it (setDerived), and
// a call keeps only its view of it (offsetView: which of the set's
// recordings it reads, and how F gave each), a few bytes a recording, never
// a copy of the set's places. Each handler once built an index of its own of
// every earlier place of the set and kept it for the whole call, waiting
// too: api-server's round (14 813 Mongo recordings, 93 pooled handlers in
// the learn step at once) held 3.4-4.7 GB of them.
//
// It holds the places of a recording the set keeps (the held part's, and
// those Order.GivenPlaces keeps for the set) as they are, and counts its own
// part, and the places of a recording the set does not keep, within budget
// (offsetBudget); a call that would take it past its budget reads its
// recordings into an index of its own instead (no budget: as with no set),
// which it drops with the call. Its lists and each list's places are
// replaced, never changed, once read, so a call reads what it took (its
// offsetSnap) without a lock while another adds the recordings it reads.
type offsetIndex struct {
	mu     sync.Mutex
	of     map[string]int32 // by offsetSource.key: recs
	recs   []offsetRec
	byAt   []int32      // recs by at, then as taken
	lists  []offsetList // by key, in keyLess's order
	pos    map[offsetKey]int
	bytes  int64
	budget int64 // 0: none (a call's own)
	// reads are the recordings whose places it took, views the calls that
	// read it: for the rows.
	reads, views int
}

// offsetBudget is the bytes a set's offset index counts (offsetIndex): about
// 8 bytes an instant and a hundred a recording past the places the set keeps,
// and the places of a recording it does not keep.
var offsetBudget int64 = 64 << 20

// The sizes offsetIndex counts: a place of a list (its ref and its moment),
// and a recording's entry past its key (with its place in byAt).
const (
	offsetRefBytes = 16
	offsetRecBytes = 116
)

// offsetRef is a place of an offsetList: the index's recording (rec) and
// which of its places (i).
type offsetRef struct{ rec, i int32 }

// offsetRec is a recording of the index: its mock, the call it answers
// (callOf), when its request was recorded, and its places (keyedPlaces).
type offsetRec struct {
	mock, call string
	at         time.Time
	places     []keyedPlace
}

// offsetList is the index's places at one key, by the call their recordings
// answer (offsetGroup, by call): the offset rule pairs a place only with
// another of its call's (offsetFrom).
type offsetList struct {
	k      offsetKey
	groups []offsetGroup
}

// offsetGroup is a list's places of one call, by moment, then in the order
// the index took them (by recording, then by place), and their moments, in
// that order: the offset rule searches a place's move among them.
type offsetGroup struct {
	call string
	refs []offsetRef
	at   []int64
}

// offsetSnap is what a call read of the index: its recordings, its
// recordings by when their requests were recorded (byAt, then as taken), and
// its lists, as they stood, which the index never changes.
type offsetSnap struct {
	recs  []offsetRec
	byAt  []int32
	lists []offsetList
}

func (s offsetSnap) momentOf(r offsetRef) int64 { return s.recs[r.rec].places[r.i].m.key.at }

// offsetView is how a call reads one of the index's recordings: whether it
// reads it (in), whether F gave it the application before the call (given)
// and where in the set (first, last: Raw.ServedIn), whether keploy cannot
// read when it was recorded (noTime: Raw.noTime), and whether it reads it as
// one F gave earlier in the set that none of the call's payloads is
// (other: GivenEarlier).
type offsetView struct {
	in, given, noTime, other bool
	first, last              int32
}

// offsetSource is a recording a call's offset rule reads (offsetPlacesOf),
// as one of its readings hands it: a payload (x: one F served before the call
// or recorded before it, or an exchange's served leaf by leaf) or one F gave
// earlier in the set (g); key names its recording (its mock and when its
// request was recorded), as the readings of one recording are one. places
// and kept are what offsetIndex.take read of it.
type offsetSource struct {
	key         string
	x           *Raw
	g           *GivenPlaces
	given       bool
	first, last int
	places      []keyedPlace
	kept, read  bool
}

func offsetKeyOf(mock string, at time.Time) string { return mock + "\x00" + at.String() }

// readPlaces reads s's places, and whether the set keeps them (setPlaces,
// Order.GivenPlaces), once.
func (s *offsetSource) readPlaces(dc *DriftCache) {
	if s.read {
		return
	}
	s.read = true
	if s.g != nil {
		s.places, s.kept = s.g.places, true
		return
	}
	s.places = dc.keyedPlaces(*s.x)
	s.kept = dc.placesKept(*s.x)
}

func newOffsetIndex(budget int64) *offsetIndex {
	return &offsetIndex{of: map[string]int32{}, pos: map[offsetKey]int{}, budget: budget}
}

// offsetIndex is the set's offset index (setDerived.offsets), or one of the
// call's own where dc keeps no set.
func (dc *DriftCache) offsetIndex() *offsetIndex {
	if dc != nil && dc.set != nil {
		return dc.set.offsets
	}
	return newOffsetIndex(0)
}

// keyLess is the order offsetFrom reads the places' keys in.
func keyLess(a, b offsetKey) bool {
	if a.path != b.path {
		return a.path < b.path
	}
	if a.form != b.form {
		return a.form < b.form
	}
	if a.layout != b.layout {
		return a.layout < b.layout
	}
	return a.label < b.label
}

// take is the index's recording of each of srcs, by srcs' order (-1 of one
// it could not add), and what the call reads of the index: each it does not hold yet read (outside its lock,
// as reading one may wait for another handler's read of it: derive) and
// added. ok is false where adding one would take the index past its budget:
// the call reads its recordings into one of its own (offsetPlacesOf).
func (ix *offsetIndex) take(dc *DriftCache, srcs []offsetSource) (offsetSnap, []int32, bool) {
	recs := make([]int32, len(srcs))
	ix.mu.Lock()
	var missing []int
	for i := range srcs {
		if r, ok := ix.of[srcs[i].key]; ok {
			recs[i] = r
			continue
		}
		recs[i] = -1
		missing = append(missing, i)
	}
	ix.mu.Unlock()
	for _, i := range missing {
		srcs[i].readPlaces(dc)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ok := true
	first := int32(len(ix.recs))
	added := map[offsetKey][]offsetRef{}
	for _, i := range missing {
		s := &srcs[i]
		if r, found := ix.of[s.key]; found {
			recs[i] = r
			continue
		}
		size := int64(offsetRecBytes + len(s.key) + offsetRefBytes*len(s.places))
		if !s.kept {
			size += placesBytes(s.places)
		}
		if ix.budget > 0 && ix.bytes+size > ix.budget {
			ok = false
			continue
		}
		r := int32(len(ix.recs))
		rec := offsetRec{places: s.places}
		if s.g != nil {
			rec.mock, rec.call, rec.at = s.g.mock, s.g.call, s.g.at
		} else {
			rec.mock, rec.call, rec.at = s.x.mock, s.x.call, s.x.at
		}
		ix.recs = append(ix.recs, rec)
		ix.of[s.key] = r
		ix.bytes += size
		ix.reads++
		recs[i] = r
		for pi, kp := range s.places {
			added[kp.k] = append(added[kp.k], offsetRef{rec: r, i: int32(pi)})
		}
	}
	ix.addLists(added)
	ix.addByAt(first)
	ix.views++
	return offsetSnap{recs: ix.recs, byAt: ix.byAt, lists: ix.lists}, recs, ok
}

// addByAt puts the recordings from first on, just added, into byAt,
// replacing it, never changing the one a call reads.
func (ix *offsetIndex) addByAt(first int32) {
	n := int32(len(ix.recs))
	if first >= n {
		return
	}
	fresh := make([]int32, 0, n-first)
	for r := first; r < n; r++ {
		fresh = append(fresh, r)
	}
	before := func(a, b int32) bool {
		if !ix.recs[a].at.Equal(ix.recs[b].at) {
			return ix.recs[a].at.Before(ix.recs[b].at)
		}
		return a < b
	}
	sort.SliceStable(fresh, func(i, j int) bool { return before(fresh[i], fresh[j]) })
	merged := make([]int32, 0, len(ix.byAt)+len(fresh))
	i, j := 0, 0
	for i < len(ix.byAt) || j < len(fresh) {
		if j == len(fresh) || i < len(ix.byAt) && before(ix.byAt[i], fresh[j]) {
			merged = append(merged, ix.byAt[i])
			i++
			continue
		}
		merged = append(merged, fresh[j])
		j++
	}
	ix.byAt = merged
}

// addLists puts added, places of recordings just added, into the index's
// lists, each in its call's group by moment after those it holds at the same
// moment, replacing every group and list it changes and the lists
// themselves, never changing one a call reads.
func (ix *offsetIndex) addLists(added map[offsetKey][]offsetRef) {
	if len(added) == 0 {
		return
	}
	snap := offsetSnap{recs: ix.recs}
	lists := append([]offsetList(nil), ix.lists...)
	fresh := false
	for k, refs := range added {
		sort.SliceStable(refs, func(a, b int) bool { return snap.momentOf(refs[a]) < snap.momentOf(refs[b]) })
		var calls []string
		byCall := map[string][]offsetRef{}
		for _, r := range refs {
			c := snap.recs[r.rec].call
			if _, seen := byCall[c]; !seen {
				calls = append(calls, c)
			}
			byCall[c] = append(byCall[c], r)
		}
		p, ok := ix.pos[k]
		var groups []offsetGroup
		if ok {
			groups = append([]offsetGroup(nil), lists[p].groups...)
		}
		for _, c := range calls {
			gi := sort.Search(len(groups), func(i int) bool { return groups[i].call >= c })
			if gi < len(groups) && groups[gi].call == c {
				groups[gi] = snap.mergeGroup(groups[gi], byCall[c])
				continue
			}
			groups = append(groups, offsetGroup{})
			copy(groups[gi+1:], groups[gi:])
			groups[gi] = snap.mergeGroup(offsetGroup{call: c}, byCall[c])
		}
		if ok {
			lists[p].groups = groups
			continue
		}
		lists = append(lists, offsetList{k: k, groups: groups})
		fresh = true
	}
	if fresh {
		sort.Slice(lists, func(a, b int) bool { return keyLess(lists[a].k, lists[b].k) })
		for i, l := range lists {
			ix.pos[l.k] = i
		}
	}
	ix.lists = lists
}

// mergeGroup is g with refs, by moment, put in after those g holds at the
// same moment: a new group.
func (s offsetSnap) mergeGroup(g offsetGroup, refs []offsetRef) offsetGroup {
	out := offsetGroup{call: g.call, refs: make([]offsetRef, 0, len(g.refs)+len(refs)), at: make([]int64, 0, len(g.refs)+len(refs))}
	i, j := 0, 0
	for i < len(g.refs) || j < len(refs) {
		if j == len(refs) || i < len(g.refs) && g.at[i] <= s.momentOf(refs[j]) {
			out.refs, out.at = append(out.refs, g.refs[i]), append(out.at, g.at[i])
			i++
			continue
		}
		out.refs, out.at = append(out.refs, refs[j]), append(out.at, s.momentOf(refs[j]))
		j++
	}
	return out
}

// listOf is the index in s.lists of k's list, ok false where s holds none.
func (s offsetSnap) listOf(k offsetKey) (int, bool) {
	i := sort.Search(len(s.lists), func(i int) bool { return !keyLess(s.lists[i].k, k) })
	return i, i < len(s.lists) && s.lists[i].k == k
}

// group is l's group of call, nil where l holds none.
func (l *offsetList) group(call string) *offsetGroup {
	i := sort.Search(len(l.groups), func(i int) bool { return l.groups[i].call >= call })
	if i < len(l.groups) && l.groups[i].call == call {
		return &l.groups[i]
	}
	return nil
}

// within is the places of g whose moments are in (lo, hi), from-to: all of
// them where dated is false (a time of day, read around the day).
func (g *offsetGroup) within(dated bool, lo, hi int64) (from, to int) {
	if !dated {
		return 0, len(g.at)
	}
	from = sort.Search(len(g.at), func(j int) bool { return g.at[j] > lo })
	to = sort.Search(len(g.at), func(j int) bool { return g.at[j] >= hi })
	return from, max(from, to)
}

// placesBytes is the bytes the held part counts of places (setPlaces).
func placesBytes(places []keyedPlace) int64 {
	size := int64(headerBytes)
	for _, k := range places {
		size += placeBytes + int64(len(k.k.path)+len(k.k.label)+len(k.where))
	}
	return size
}

// placesKept reports whether the set keeps x's places (setPlaces), which the
// offset index then holds as they are.
func (dc *DriftCache) placesKept(x Raw) bool {
	s := dc.setOf(x)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held.find(x.id, partPlaces) != nil
}

// offsetBytes is the bytes the index counts, for the rows.
func (ix *offsetIndex) offsetBytes() int64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.bytes
}

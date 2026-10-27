package proxy

import (
	"slices"
	"sort"
	"strconv"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// keyedExamined counts the tree entries keyed walks examined (rangeKeyed,
// exactView.rangeShownKeyed, and a view filing what it shows of a key,
// extraLocked): what a keyed read costs, read by the rows that pin it. shownBuilds counts the times an exact view's keyed walk filed what
// it shows of a tree under a key (keyPart and the view's extras): once per
// view, tree generation and key, never on a call.
var keyedExamined, shownBuilds atomic.Uint64

// viewNarrowed counts the staged mocks an exact view's reads were handed to
// narrow to what it shows (keep, reusable, teardown): what an unkeyed read
// costs beside the tree's split, read by the rows that pin it. A tier read
// hands them what the view shows of the tier (shownOf), never its values.
var viewNarrowed atomic.Uint64

// tierReadsWalk, set only by this package's rows, makes shownOf hand a tier's
// values of the kind, as the view's reads were handed them before it: the
// oracle a row compares each read with.
var tierReadsWalk atomic.Bool

// keyParts is a tree's split, at one generation (TreeDb.gen), of the entries a
// MockIndex files under each key a walk asked for. Built under the tree's
// write lock, read under its read lock, and dropped whole when the generation
// moves.
type keyParts struct {
	gen   uint64
	lists map[string]*keyPart
}

// keyPart is the split of the entries filed under one key, each in tree order
// and live when it was built: phase holds the protocol-phase ones, which every
// exact view shows; rest holds every other, and byName their positions in
// rest by mock name, so a view finds the few of them it shows (its worker
// scope, its pool) without examining the rest.
type keyPart struct {
	phase  []keyedEntry
	rest   []keyedEntry
	byName map[string][]int
}

// storedLocked moves the tree's generation: every change that stores a value
// under a key (insert, swapValue, update, deleteAll) calls it, holding db.mu
// for writing. A removal does not: an entry filed before it is skipped by the
// liveness check every walk makes (liveLocked).
func (db *TreeDb) storedLocked() {
	db.gen++
	db.parts = nil
}

// rlockPart returns db's split of what ix files under key at the tree's
// current generation, holding db.mu for reading; the caller releases it. The
// first call at a generation builds it under the write lock.
func (db *TreeDb) rlockPart(ix *integrations.MockIndex, key string) *keyPart {
	for {
		db.mu.RLock()
		if ps := db.parts[ix]; ps != nil && ps.gen == db.gen {
			if p := ps.lists[key]; p != nil {
				return p
			}
		}
		db.mu.RUnlock()
		db.mu.Lock()
		db.buildPartLocked(ix, key)
		db.mu.Unlock()
	}
}

// buildPartLocked files db's split of what ix files under key, building the
// key index first when it has none. db.mu must be held for writing.
func (db *TreeDb) buildPartLocked(ix *integrations.MockIndex, key string) {
	ps := db.parts[ix]
	if ps != nil && ps.gen == db.gen && ps.lists[key] != nil {
		return
	}
	if db.keyed[ix] == nil {
		if db.keyed == nil {
			db.keyed = map[*integrations.MockIndex]*keyIndex{}
		}
		db.keyed[ix] = db.buildKeyIndexLocked(ix)
	}
	if ps == nil || ps.gen != db.gen {
		ps = &keyParts{gen: db.gen, lists: map[string]*keyPart{}}
		if db.parts == nil {
			db.parts = map[*integrations.MockIndex]*keyParts{}
		}
		db.parts[ix] = ps
	}
	shownBuilds.Add(1)
	p := &keyPart{byName: map[string][]int{}}
	for _, e := range db.keyed[ix].lists[key] {
		if !db.liveLocked(e) {
			continue
		}
		if e.mk.IsProtocolPhase() {
			p.phase = append(p.phase, e)
			continue
		}
		p.byName[e.mk.Name] = append(p.byName[e.mk.Name], len(p.rest))
		p.rest = append(p.rest, e)
	}
	ps.lists[key] = p
}

// shownAt names one tree's entries under one key of one MockIndex, for the
// view's own extras (exactView.shown).
type shownAt struct {
	tree *TreeDb
	ix   *integrations.MockIndex
	key  string
}

// shownExtra is what a view shows of a keyPart's rest at one tree
// generation, in tree order.
type shownExtra struct {
	gen   uint64
	extra []keyedEntry
}

// showsStatic is shows less the spends: what the view shows of mk for what mk
// is and what the view was installed with, neither of which changes while the
// view is in force. A mock in a tree is not changed in place once it is
// pooled (models.Mock.MarkPooled): a change stores a new value, and moves the
// tree's generation.
func (v *exactView) showsStatic(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	if mk.IsProtocolPhase() {
		return true
	}
	return v.showsStaticRest(mk)
}

// showsStaticRest is showsStatic of a mock that is not protocol-phase.
func (v *exactView) showsStaticRest(mk *models.Mock) bool {
	if v.isWorker(mk.Name) {
		return true
	}
	if v.test != "" {
		// Under the background rules the pool answers in a test too,
		// after the test's own and the worker scope (keep); an async
		// lane's recording keeps today's rules.
		return v.bg.inPool(mk.Name) && !mk.IsAsync()
	}
	_, ok := v.pool[mk.Name]
	return ok
}

// unspentNow reports whether mk is neither spent in the view nor consumed
// outside every test in the staging: the part of shows that changes while the
// view is in force.
func (v *exactView) unspentNow(mk *models.Mock) bool {
	return !v.spentIn(mk.Name) && !v.spentOutside(mk.Name)
}

// spentIn reports whether a parser spent name in v (exactView.spent). It takes
// no lock.
func (v *exactView) spentIn(name string) bool {
	_, spent := v.spent.Load(name)
	return spent
}

// extraLocked returns what v shows of p.rest, the part of db's entries under
// key that are not protocol-phase, at db's generation gen, built on its first
// use at that generation. db.mu must be held, for reading at least.
//
// The view's names are few beside the set (its worker scope and its pool), so
// it looks each one up in p.byName rather than examining p.rest, unless rest
// is the smaller.
func (v *exactView) extraLocked(db *TreeDb, ix *integrations.MockIndex, key string, p *keyPart, gen uint64) []keyedEntry {
	at := shownAt{db, ix, key}
	v.shownMu.Lock()
	defer v.shownMu.Unlock()
	if s := v.shown[at]; s != nil && s.gen == gen {
		return s.extra
	}
	shownBuilds.Add(1)
	var pool map[string]struct{}
	if v.test != "" {
		if v.bg != nil {
			pool = v.bg.pool
		}
	} else {
		pool = v.pool
	}
	var extra []keyedEntry
	if looks := len(v.worker) + len(pool); looks < len(p.rest) {
		keyedExamined.Add(uint64(looks))
		var pos []int
		for name := range v.worker {
			pos = append(pos, p.byName[name]...)
		}
		for name := range pool {
			pos = append(pos, p.byName[name]...)
		}
		slices.Sort(pos)
		pos = slices.Compact(pos)
		for _, i := range pos {
			if e := p.rest[i]; v.showsStaticRest(e.mk) {
				extra = append(extra, e)
			}
		}
	} else {
		keyedExamined.Add(uint64(len(p.rest)))
		for _, e := range p.rest {
			if v.showsStaticRest(e.mk) {
				extra = append(extra, e)
			}
		}
	}
	if v.shown == nil {
		v.shown = map[shownAt]*shownExtra{}
	}
	v.shown[at] = &shownExtra{gen: gen, extra: extra}
	return extra
}

// rangeShownKeyed is db.rangeKeyed(ix, key, ·) narrowed to the mocks v shows:
// it calls fn, in tree order, with each mock db holds that ix files under key
// and v shows, until fn returns false. It examines only those: the
// protocol-phase entries the tree split out at its generation (keyPart), and
// the few others the view shows (extraLocked), never the open test's own
// recordings the tree also files there.
//
// Like rangeKeyed, fn runs without db.mu held, a few entries at a time, and a
// walk resumes after the tree key of the last entry it examined, rebuilding
// both lists when the tree's generation moved in between. Whether the view
// spent a mock is read as fn is about to be called with it, as shows reads it.
func (v *exactView) rangeShownKeyed(db *TreeDb, ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) {
	const batch = 16
	cmp := db.rbt.Comparator
	var (
		after interface{}
		buf   = make([]*models.Mock, 0, batch)
	)
	first := true
	for {
		buf = buf[:0]
		p := db.rlockPart(ix, key)
		extra := v.extraLocked(db, ix, key, p, db.gen)
		var want, got []*models.Mock
		verify := first && exactViewVerify.Load()
		if verify {
			want, got = v.recomputeShownLocked(db, ix, key, p, extra)
		}
		first = false
		i, j := 0, 0
		if after != nil {
			i = sort.Search(len(p.phase), func(k int) bool { return cmp(p.phase[k].key, after) > 0 })
			j = sort.Search(len(extra), func(k int) bool { return cmp(extra[k].key, after) > 0 })
		}
		examined := 0
		for (i < len(p.phase) || j < len(extra)) && len(buf) < batch {
			var e keyedEntry
			if j >= len(extra) || (i < len(p.phase) && cmp(p.phase[i].key, extra[j].key) < 0) {
				e = p.phase[i]
				i++
			} else {
				e = extra[j]
				j++
			}
			examined++
			after = e.key
			if db.liveLocked(e) {
				buf = append(buf, e.mk)
			}
		}
		more := i < len(p.phase) || j < len(extra)
		db.mu.RUnlock()
		if verify {
			verifyExactRead("the staged mocks the view shows under key "+strconv.Quote(key), want, got)
		}
		keyedExamined.Add(uint64(examined))
		for _, mk := range buf {
			if v.unspentNow(mk) && !fn(mk) {
				return
			}
		}
		if !more {
			return
		}
	}
}

// recomputeShownLocked gives, for the verify mode, what rangeShownKeyed reads
// from p and extra (got), and the same read the way the walk made it before
// the tree split it (want): every live entry the key index files under key
// that the view shows, spends aside (both read those the same way, as fn is
// called). db.mu must be held, so no change falls between the two; the
// caller compares them once it is released (verifyExactRead panics).
func (v *exactView) recomputeShownLocked(db *TreeDb, ix *integrations.MockIndex, key string, p *keyPart, extra []keyedEntry) (want, got []*models.Mock) {
	if ki := db.keyed[ix]; ki != nil {
		for _, e := range ki.lists[key] {
			if db.liveLocked(e) && v.showsStatic(e.mk) {
				want = append(want, e.mk)
			}
		}
	}
	cmp := db.rbt.Comparator
	i, j := 0, 0
	for i < len(p.phase) || j < len(extra) {
		var e keyedEntry
		if j >= len(extra) || (i < len(p.phase) && cmp(p.phase[i].key, extra[j].key) < 0) {
			e = p.phase[i]
			i++
		} else {
			e = extra[j]
			j++
		}
		if db.liveLocked(e) {
			got = append(got, e.mk)
		}
	}
	return want, got
}

// kindIndex files every mock under "" and under its kind, so an exact view's
// unkeyed read of a tier (shownOf) goes through the split its keyed reads use
// (keyPart, extraLocked): what it examines is what the view shows of the
// tier, never the whole tier, whose values under keploy mock replay are every
// test's recordings.
var kindIndex = &integrations.MockIndex{Keys: func(mk *models.Mock) []string {
	return []string{"", string(mk.Kind)}
}}

// shownOf is, of db's values of kind (every kind for ""), in tree order, those
// v shows for what each is and what v was installed with (showsStatic): the
// protocol-phase ones, and the few of its own names (its worker scope, its
// pool). The spends are not read here: the view's reads (keep, reusable,
// perTest) narrow the list with shows, as they narrowed the tier's values, and
// get the same list from it, since shows is showsStatic and unspentNow. nil
// for a nil tree.
//
// It examines the protocol-phase entries the tree split out at its generation
// and the view's extras, built once per view and generation (extraLocked);
// the tree's split is built once per generation, on the first read, by one
// walk of the tree. In the verify mode the read is recomputed by that walk,
// under the same lock, and a difference panics.
func (v *exactView) shownOf(db *TreeDb, kind models.Kind) []*models.Mock {
	if db == nil {
		return nil
	}
	if tierReadsWalk.Load() {
		var all []*models.Mock
		db.rangeValues(func(x interface{}) bool {
			if mk, ok := x.(*models.Mock); ok && mk != nil && (kind == "" || mk.Kind == kind) {
				all = append(all, mk)
			}
			return true
		})
		return all
	}
	key := string(kind)
	p := db.rlockPart(kindIndex, key)
	extra := v.extraLocked(db, kindIndex, key, p, db.gen)
	out := make([]*models.Mock, 0, len(p.phase)+len(extra))
	cmp := db.rbt.Comparator
	i, j := 0, 0
	for i < len(p.phase) || j < len(extra) {
		var e keyedEntry
		if j >= len(extra) || (i < len(p.phase) && cmp(p.phase[i].key, extra[j].key) < 0) {
			e = p.phase[i]
			i++
		} else {
			e = extra[j]
			j++
		}
		if db.liveLocked(e) {
			out = append(out, e.mk)
		}
	}
	var want []*models.Mock
	verify := exactViewVerify.Load()
	if verify {
		it := db.rbt.Iterator()
		for it.Next() {
			if mk, ok := it.Value().(*models.Mock); ok && mk != nil && (kind == "" || mk.Kind == kind) && v.showsStatic(mk) {
				want = append(want, mk)
			}
		}
	}
	db.mu.RUnlock()
	keyedExamined.Add(uint64(len(p.phase) + len(extra)))
	if verify {
		verifyExactRead("the staged mocks of kind "+strconv.Quote(key)+" the view shows", want, out)
	}
	return out
}

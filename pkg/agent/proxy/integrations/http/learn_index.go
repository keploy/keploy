package http

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// learnIndex is the learn step's index of the HTTP recordings a store's
// reads gave at one staging generation (integrations.StagingGenerations):
// each recording learnedMatch's scope would place, in the scope's order (the
// per-test tier's, then the session tier's, each in recorded order), filed
// under its method and its path with keploy.yml's url noise masked. A
// recording a segment of whose path may be excused (a learned place of its
// URL, a read of the clock at one, or obfuscation noise: learnSieve.shapes)
// is filed under its method and its segment count too, since a call of
// another path may still be it. So a learned call reads the recordings that
// may be it, not the whole staged set: learnSieve passes no other.
//
// While the generation holds, the store's reads are what they were when it
// began, less the mocks MockSpent reports (a consume under exact serving), so
// the index, less those, is what the reads would give: the call's scope is
// built from it with the call's own account of whose each recording is and
// which answered, as from a read.
//
// While a test is open the index is of the shared part of its reads
// (integrations.SharedReads), kept across tests while its SharedGen holds: a
// test's open or close moves the staging generation but not what every
// test's view shows beside its own recordings (the protocol-phase ones, the
// worker scope, the background pool). The test's unanswered recordings,
// which lead each read, are read for each call beside it (learnLook).
type learnIndex struct {
	store   integrations.MockMemDb
	gen     uint64
	url     string
	entries []learnEntry
	// split: entries[:split] are the per-test read's, the rest the session
	// read's.
	split  int
	byPath map[string][]int
	wild   map[string][]int
}

// learnEntry is one recording of a learnIndex: its tier and its path as the
// learn step reads it (maskedPath).
type learnEntry struct {
	m       *models.Mock
	perTest bool
	masked  string
}

// learnIndexes keeps the last learnIndex an HTTP parser built (genIndexes):
// of a store's reads at one staging generation, and of the shared part of an
// open test's reads at one SharedGen (integrations.SharedReads).
type learnIndexes struct {
	genIndexes[*learnIndex]
	shared genIndexes[*learnIndex]
	// owned is the index of the open test's own recordings beside shared.
	owned ownedIndexes[*learnOwned]
}

// genIndexes keeps the last index of a store's reads an HTTP parser built at
// one staging generation, and the generation its last call read without one:
// an index is built on the second call that reads the same store at the same
// generation, so a store whose generation moves on every call (one not under
// exact serving) is walked as before and never pays for an index it would
// not reuse.
type genIndexes[T any] struct {
	mu   sync.Mutex
	last T
	// key is last's; has is false while none is kept.
	key  learnIndexKey
	has  bool
	seen learnIndexKey
}

// learnIndexKey names what an index is of: a store (base), and its staging
// generation (gen) or the shared generation of an open test's reads
// (shared), with url the url noise its paths are masked with.
type learnIndexKey struct {
	store  integrations.MockMemDb
	gen    uint64
	shared integrations.SharedGen
	url    string
}

// stagedKey is the key of db's reads at its staging generation now: false
// where it keeps none, or is held to a staging that moved
// (integrations.RestagedGeneration: it reads nothing).
func stagedKey(db integrations.MockMemDb) (learnIndexKey, bool) {
	gen, ok := integrations.StagingGenerationOf(db, models.HTTP)
	if !ok || gen == integrations.RestagedGeneration {
		return learnIndexKey{}, false
	}
	return learnIndexKey{gen: gen}, true
}

// sharedKey is the key of the shared part of db's reads now: false where no
// test is open, or db is held to a staging that moved (SharedReads).
func sharedKey(db integrations.MockMemDb) (learnIndexKey, bool) {
	g, ok := integrations.SharedGenerationOf(db, models.HTTP)
	if !ok {
		return learnIndexKey{}, false
	}
	return learnIndexKey{shared: g}, true
}

// of is the index of what db reads now, for base (the store db is held from:
// a store held to a call is made for each call, and shares base's reads while
// its staging is the call's), with url the url noise its paths are masked
// with; false where the call reads the store instead: db is not under exact
// serving (a consume would move the generation), keeps no staging
// generation, is held to a staging that moved, base cannot be told apart
// from another store by identity, or this is the first call to read base at
// this generation. read is the store's read the index is built from, and
// build builds it from that read.
func (x *genIndexes[T]) of(base, db integrations.MockMemDb, url string, read func() error, build func(learnIndexKey) T) (T, bool) {
	ix, _, ok := x.keyed(base, db, url, stagedKey, false, read, build)
	return ix, ok
}

// sharedOf is of for the shared part of an open test's reads
// (integrations.SharedReads), keyed on its SharedGen: read reads SharedMocks.
// The index is built on the first call that reads it, since a SharedGen
// moves on no consume and on no test's open or close, and is kept across
// tests while it holds. The key it returns is the index's.
func (x *genIndexes[T]) sharedOf(base, db integrations.MockMemDb, url string, read func() error, build func(learnIndexKey) T) (T, learnIndexKey, bool) {
	return x.keyed(base, db, url, sharedKey, true, read, build)
}

// keyed is of with the generation sample takes of db: the index kept for
// base at that key, or one built from read, then, when sample takes the same
// key again after the read (what it read is of that one generation); on the
// second call at the key, or on the first with eager.
func (x *genIndexes[T]) keyed(base, db integrations.MockMemDb, url string, sample func(integrations.MockMemDb) (learnIndexKey, bool), eager bool, read func() error, build func(learnIndexKey) T) (T, learnIndexKey, bool) {
	var none T
	if !integrations.ExactServing(db) || base == nil || reflect.ValueOf(base).Kind() != reflect.Pointer {
		return none, learnIndexKey{}, false
	}
	key, ok := sample(db)
	if !ok {
		return none, learnIndexKey{}, false
	}
	key.store, key.url = base, url
	x.mu.Lock()
	if x.has && x.key == key {
		ix := x.last
		x.mu.Unlock()
		return ix, key, true
	}
	second := x.seen == key
	x.seen = key
	// The kept index is of another store, generation or url noise: let go
	// of it, and of the recordings it holds, now rather than at the next
	// build.
	x.last, x.key, x.has = none, learnIndexKey{}, false
	x.mu.Unlock()
	// And when base lets its recordings go (a staging, a store into a tree,
	// a delete, its Close), let go of what is kept of base then, rather than
	// at the next call, which may come only in a later set, or never.
	integrations.WhenRestaged(base, x, func() { x.release(base) })
	if !second && !eager {
		return none, learnIndexKey{}, false
	}
	if read() != nil {
		return none, learnIndexKey{}, false
	}
	if again, ok := sample(db); !ok || again.gen != key.gen || again.shared != key.shared {
		// The generation moved under the read: what it read is of no one
		// generation.
		return none, learnIndexKey{}, false
	}
	ix := build(key)
	x.mu.Lock()
	x.last, x.key, x.has = ix, key, true
	x.mu.Unlock()
	if again, ok := sample(db); !ok || again.gen != key.gen || again.shared != key.shared {
		// base let its recordings go between the read and the keep: its
		// release ran before this index was kept (the store moves its
		// generation before it runs them).
		x.release(base)
	}
	return ix, key, true
}

// release lets go of what x keeps of store: its index, and the key of its
// last call, which names the store.
func (x *genIndexes[T]) release(store integrations.MockMemDb) {
	var none T
	x.mu.Lock()
	if x.has && x.key.store == store {
		x.last, x.key, x.has = none, learnIndexKey{}, false
	}
	if x.seen.store == store {
		x.seen = learnIndexKey{}
	}
	x.mu.Unlock()
}

// learnIndexBuilds counts the indexes built: what the rows count to see that
// one generation builds one.
var learnIndexBuilds atomic.Int64

// learnVisits counts the recordings a learned call's scope placed (read its
// parts and destination for): what the rows count to see that a call reads
// only its bucket of the staged set.
var learnVisits atomic.Int64

// urlNoiseKey names keploy.yml's url noise, which the masked paths an index
// files recordings under are read with.
func urlNoiseKey(url []*regexp.Regexp) string {
	var b strings.Builder
	for _, re := range url {
		b.WriteString(strconv.Quote(re.String()))
	}
	return b.String()
}

// learnLook is the learn step's indexed read for one call: an index, and for
// an index of the shared part of an open test's reads, the test's unanswered
// recordings the call may be, read for it through the index of the test's
// own (learnOwned), in recorded order (owned), which lead each tier's read
// (integrations.SharedReads). gen is the staging generation the read is of.
type learnLook struct {
	ix     *learnIndex
	shared bool
	owned  []learnEntry
	gen    uint64
}

// of is the learn step's indexed read of what db reads now, for base, for a
// call of method whose masked path is masked, or false where the call reads
// the store instead. While a test is open it is the index of the shared part
// of its reads (genIndexes.sharedOf, built from SharedMocks) beside the
// test's unanswered recordings the call may be, read now through the index
// of the test's own (ownedIndexes); otherwise the index of db's staging
// generation (genIndexes.of), built from read, the store's read.
func (x *learnIndexes) of(base, db integrations.MockMemDb, n learnNoise, method, masked string, read func() (perTest, session []*models.Mock, err error)) (learnLook, bool) {
	url := urlNoiseKey(n.url)
	if sr, ok := db.(integrations.SharedReads); ok {
		var perTest, session []*models.Mock
		ix, key, ok := x.shared.sharedOf(base, db, url, func() error {
			var ok bool
			if perTest, session, ok = sr.SharedMocks(); !ok {
				return errNoSharedReads
			}
			return nil
		}, func(key learnIndexKey) *learnIndex {
			return buildLearnIndex(key, perTest, session, n)
		})
		if ok {
			own, view, owned, ok := x.owned.of(base, sr, url, func(owned []*models.Mock) *learnOwned { return buildLearnOwned(owned, n) })
			if !ok {
				return learnLook{}, false
			}
			ranks := own.ranks(method, masked)
			at := make([]int, len(ranks))
			for i, r := range ranks {
				at[i] = own.pos[r]
			}
			gen, live, ok := ownedLive(sr, key, view, at)
			if !ok {
				return learnLook{}, false
			}
			look := learnLook{ix: ix, shared: true, gen: gen}
			// live is at less the answered ones, in at's order.
			for i, k := 0, 0; i < len(ranks) && k < len(live); i++ {
				if at[i] == live[k] {
					look.owned = append(look.owned, learnEntry{m: owned[at[i]], masked: own.masked[ranks[i]]})
					k++
				}
			}
			return look, true
		}
	}
	var perTest, session []*models.Mock
	ix, ok := x.genIndexes.of(base, db, url, func() error {
		var err error
		perTest, session, err = read()
		return err
	}, func(key learnIndexKey) *learnIndex {
		return buildLearnIndex(key, perTest, session, n)
	})
	if !ok {
		return learnLook{}, false
	}
	return learnLook{ix: ix, gen: ix.gen}, true
}

// errNoSharedReads: the store offered no shared part of its reads after its
// SharedGen said it would (a test closed between the two).
var errNoSharedReads = errors.New("the store offers no shared reads now")

// buildLearnIndex files the recordings learnedMatch's scope would place from
// the reads perTest and session (scopeReads.scope), in its order.
func buildLearnIndex(key learnIndexKey, perTest, session []*models.Mock, n learnNoise) *learnIndex {
	learnIndexBuilds.Add(1)
	ix := &learnIndex{store: key.store, gen: key.gen, url: key.url, byPath: map[string][]int{}, wild: map[string][]int{}}
	add := func(mocks []*models.Mock, perTest bool) {
		for _, m := range inRecordedOrder(FilterHTTPMocks(mocks)) {
			if m == nil || m.Spec.HTTPReq == nil || hasVariables(m) {
				continue
			}
			rp := recordedParts(m)
			masked := n.maskedPath(rp.path)
			p := len(ix.entries)
			ix.entries = append(ix.entries, learnEntry{m: m, perTest: perTest, masked: masked})
			ix.byPath[rp.method+" "+masked] = append(ix.byPath[rp.method+" "+masked], p)
			if learnsWide(m) {
				w := rp.method + " " + strconv.Itoa(segmentCount(masked))
				ix.wild[w] = append(ix.wild[w], p)
			}
		}
	}
	add(perTest, true)
	ix.split = len(ix.entries)
	add(session, false)
	return ix
}

// learnsWide reports whether a segment of m's path may be excused
// (learnSieve.shapes): an index files it under its segment count too.
func learnsWide(m *models.Mock) bool {
	return learnsURL(m) || len(m.Noise) > 0
}

// positions are the entries a call of method whose masked path is masked may
// be, in the scope's order: those filed under its path and those whose path
// may be excused at a segment, of its segment count.
func (ix *learnIndex) positions(method, masked string) []int {
	exact, wild := ix.byPath[method+" "+masked], ix.wild[method+" "+strconv.Itoa(segmentCount(masked))]
	if len(wild) == 0 {
		return exact
	}
	out := append(append(make([]int, 0, len(exact)+len(wild)), exact...), wild...)
	slices.Sort(out)
	return slices.Compact(out)
}

// entries are the entries a call of method whose masked path is masked may
// be, in the scope's order (each tier's in recorded order: inRecordedOrder),
// less those spent reports the store dropped since the generation began. Of
// an index of an open test's shared part they are each tier's read: the
// test's unanswered recordings the call may be, then the tier's shared part,
// merged in recorded order, the test's first where two are of one instant
// and name. The shared part holds none of the test's own recordings
// (integrations.SharedReads).
func (l learnLook) entries(method, masked string, spent func(string) bool) []learnEntry {
	ps := l.ix.positions(method, masked)
	if !l.shared {
		out := make([]learnEntry, 0, len(ps))
		for _, p := range ps {
			if e := l.ix.entries[p]; !spent(e.m.Name) {
				out = append(out, e)
			}
		}
		return out
	}
	// l.owned is the test's own the call may be already (learnOwned.ranks).
	owned := l.owned
	out := make([]learnEntry, 0, 2*len(owned)+len(ps))
	i := 0
	for _, perTest := range []bool{true, false} {
		var tier []learnEntry
		for ; i < len(ps) && (ps[i] < l.ix.split) == perTest; i++ {
			e := l.ix.entries[ps[i]]
			if spent(e.m.Name) {
				continue
			}
			tier = append(tier, e)
		}
		out = mergeRecorded(out, owned, tier, perTest)
	}
	return out
}

// mergeRecorded appends to out the entries owned (each of the tier perTest
// says) and shared, each in recorded order, merged in recorded order: what
// inRecordedOrder gives of owned then shared.
func mergeRecorded(out, owned, shared []learnEntry, perTest bool) []learnEntry {
	i, j := 0, 0
	for i < len(owned) || j < len(shared) {
		if i < len(owned) && (j == len(shared) || !recordedFirst(shared[j].m, owned[i].m)) {
			e := owned[i]
			e.perTest = perTest
			out = append(out, e)
			i++
			continue
		}
		out = append(out, shared[j])
		j++
	}
	return out
}

// scope is r's scope for the live request, as scopeReads.scope builds it
// from a read with sieve set (learnedGroup), from entries, the index's
// entries the call may be (learnLook.entries). The candidates it leaves out
// are those the sieve shapes none of, which exactGroup never takes; the own
// recordings that answered and that the store let go are spent, never
// shaped, and left out too.
func (ix *learnIndex) scope(r scopeReads, entries []learnEntry, live httpParts, liveShape learn.Shape, n learnNoise) scope {
	var sc scope
	var poolCands []learn.Candidate
	var poolMocks []*models.Mock
	for _, e := range entries {
		learnVisits.Add(1)
		rp := recordedParts(e.m)
		d := destOf(live, rp)
		if operationKey(rp.method, d, segmentCount(e.masked)) != liveShape.Key {
			continue
		}
		c, own := r.candidateOf(e.m, e.perTest, rp, d, e.masked, n, true)
		if own {
			sc.cands, sc.mocks, sc.own = append(sc.cands, c), append(sc.mocks, e.m), append(sc.own, true)
		} else {
			poolCands, poolMocks = append(poolCands, c), append(poolMocks, e.m)
		}
	}
	sc.cands, sc.mocks = append(sc.cands, poolCands...), append(sc.mocks, poolMocks...)
	for range poolCands {
		sc.own = append(sc.own, false)
	}
	return sc
}

// mockSpentOf is db's MockSpent, or none spent where db keeps no staging
// generation.
func mockSpentOf(db integrations.MockMemDb) func(string) bool {
	if sg, ok := db.(integrations.StagingGenerations); ok {
		return sg.MockSpent
	}
	return func(string) bool { return false }
}

// unspentOnce hands each list a verify compares back less the recordings
// gone reports (what the store reports spent), reading gone once for each
// recording, by its name, for every list. Another call's consume can land
// between two reads of one recording's account (two identical calls at
// once, the call recorded twice): read twice, the recording would be dropped
// from one list and kept in the other, a difference the index never made.
// Read once, after the verify's read of the store, it is dropped from both
// or from neither: what the store reports spent only grows while a staging
// generation holds, so a recording the index found and the read lacks was
// spent before the read, and one the read holds was not spent when the index
// was read. A verify compares the lists only while its generation held to
// the last of these reads: one read under a later generation is of another
// view (a test's end), whose spent are not the read's.
func unspentOnce(gone func(*models.Mock) bool) func([]*models.Mock) []*models.Mock {
	read := map[string]bool{}
	return func(ms []*models.Mock) []*models.Mock {
		return slices.DeleteFunc(slices.Clone(ms), func(m *models.Mock) bool {
			was, ok := read[m.Name]
			if !ok {
				was = gone(m)
				read[m.Name] = was
			}
			return was
		})
	}
}

// learnedGroupOf is learnedGroup for the live request: through h's indexed
// read of db (learnIndexes.of) where there is one, else (and always with
// reread) from db's reads. In the exact view's verify mode a group found
// through the index is compared with the one every candidate of a read
// gives, as is its owner of each recording of it, and a difference panics.
func (h *HTTP) learnedGroupOf(base, db integrations.MockMemDb, tracker *learn.Tracker, live httpParts, liveShape learn.Shape, n learnNoise, reread bool) ([]*models.Mock, func(*models.Mock) bool, error) {
	read := func() (scopeReads, error) { return h.learnReads(db, tracker) }
	var look learnLook
	indexed := false
	sieve := n.learnSieveOf(live)
	if !reread {
		look, indexed = h.learnIx.of(base, db, n, live.method, sieve.masked, func() ([]*models.Mock, []*models.Mock, error) {
			r, err := read()
			return r.perTest, r.session, err
		})
	}
	if !indexed {
		r, err := read()
		if err != nil {
			return nil, nil, err
		}
		group, owner := r.learnedGroup(live, liveShape, n, n.learnSieveOf(live))
		return group, owner, nil
	}
	r := h.learnRules(db, tracker)
	verify := integrations.ExactViewVerify.Load()
	if verify {
		r = r.answersOnce()
	}
	r.sieve = sieve
	sc := look.ix.scope(r, look.entries(live.method, r.sieve.masked, mockSpentOf(db)), live, liveShape, n)
	group, owner := exactGroup(liveShape, sc, false), sc.owner()
	if verify {
		full, err := read()
		if err != nil {
			return nil, nil, err
		}
		// Another call's consume between the index's look and the read
		// drops a recording from the read alone, and a test's open or close
		// moves the generation: both are compared less what the store
		// reports spent after the read (unspentOnce), and only while the
		// staging generation the indexed read is of held to the last of
		// those reports.
		full.answered, full.answeredMocks = r.answered, r.answeredMocks
		fsc := full.scope(live, liveShape, n, true)
		all, allOwner := exactGroup(liveShape, fsc, false), fsc.owner()
		spentNow := mockSpentOf(db)
		unspent := unspentOnce(func(m *models.Mock) bool { return spentNow(m.Name) })
		got, want := unspent(group), unspent(all)
		if again, _ := integrations.StagingGenerationOf(db, models.HTTP); again == look.gen {
			if !slices.Equal(want, got) {
				panic(fmt.Sprintf("exact view verify: indexed learned group of %s %s: indexed %v, read %v", live.method, live.path, strictNames(got), strictNames(want)))
			}
			for _, m := range got {
				if owner(m) != allOwner(m) {
					panic(fmt.Sprintf("exact view verify: indexed learned group of %s %s: %s is the open test's own %v through the index, %v from a read", live.method, live.path, m.Name, owner(m), allOwner(m)))
				}
			}
		}
	}
	return group, owner, nil
}

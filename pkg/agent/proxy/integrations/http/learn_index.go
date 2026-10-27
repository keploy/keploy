package http

import (
	"fmt"
	"reflect"
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
type learnIndex struct {
	store   integrations.MockMemDb
	gen     uint64
	url     string
	entries []learnEntry
	byPath  map[string][]int
	wild    map[string][]int
}

// learnEntry is one recording of a learnIndex: its tier and its path as the
// learn step reads it (maskedPath).
type learnEntry struct {
	m       *models.Mock
	perTest bool
	masked  string
}

// learnIndexes keeps the last learnIndex an HTTP parser built, and the
// generation its last call read without one: an index is built on the second
// call that reads the same store at the same generation, so a store whose
// generation moves on every call (one not under exact serving) is walked as
// before and never pays for an index it would not reuse.
type learnIndexes struct {
	mu   sync.Mutex
	last *learnIndex
	seen learnIndexKey
}

type learnIndexKey struct {
	store integrations.MockMemDb
	gen   uint64
	url   string
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
func urlNoiseKey(n learnNoise) string {
	var b strings.Builder
	for _, re := range n.url {
		b.WriteString(strconv.Quote(re.String()))
	}
	return b.String()
}

// of is the index of what db reads now, for base (the store db is held from:
// a store held to a call is made for each call, and shares base's reads while
// its staging is the call's), or nil where the call reads the store instead:
// db is not under exact serving (a consume would move the generation), keeps
// no staging generation, is held to a staging that moved
// (integrations.RestagedGeneration: it reads nothing), base cannot be told
// apart from another store by identity, or this is the first call to read
// base at this generation. read is the store's read the index is built from.
func (x *learnIndexes) of(base, db integrations.MockMemDb, n learnNoise, read func() (perTest, session []*models.Mock, err error)) *learnIndex {
	if !integrations.ExactServing(db) || base == nil || reflect.ValueOf(base).Kind() != reflect.Pointer {
		return nil
	}
	gen, ok := integrations.StagingGenerationOf(db, models.HTTP)
	if !ok || gen == integrations.RestagedGeneration {
		return nil
	}
	key := learnIndexKey{store: base, gen: gen, url: urlNoiseKey(n)}
	x.mu.Lock()
	if ix := x.last; ix != nil && ix.store == base && ix.gen == gen && ix.url == key.url {
		x.mu.Unlock()
		return ix
	}
	second := x.seen == key
	x.seen = key
	// The kept index is of another store, generation or url noise: let go
	// of it, and of the recordings it holds, now rather than at the next
	// build.
	x.last = nil
	x.mu.Unlock()
	if !second {
		return nil
	}
	perTest, session, err := read()
	if err != nil {
		return nil
	}
	if again, _ := integrations.StagingGenerationOf(db, models.HTTP); again != gen {
		// The generation moved under the read: what it read is of no one
		// generation.
		return nil
	}
	ix := buildLearnIndex(key, perTest, session, n)
	x.mu.Lock()
	x.last = ix
	x.mu.Unlock()
	return ix
}

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
			if learnsURL(m) || len(m.Noise) > 0 {
				w := rp.method + " " + strconv.Itoa(segmentCount(masked))
				ix.wild[w] = append(ix.wild[w], p)
			}
		}
	}
	add(perTest, true)
	add(session, false)
	return ix
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

// scope is r's scope for the live request, as scopeReads.scope builds it
// from a read with sieve set (learnedGroup), from the index: the entries the
// call may be, less those spent reports the store dropped since the
// generation began. The candidates it leaves out are those the sieve shapes
// none of, which exactGroup never takes; the own recordings that answered
// and that the store let go are spent, never shaped, and left out too.
func (ix *learnIndex) scope(r scopeReads, spent func(string) bool, live httpParts, liveShape learn.Shape, n learnNoise) scope {
	var sc scope
	var poolCands []learn.Candidate
	var poolMocks []*models.Mock
	for _, p := range ix.positions(live.method, r.sieve.masked) {
		e := ix.entries[p]
		if spent(e.m.Name) {
			continue
		}
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

// learnedGroupOf is learnedGroup for the live request: through h's index of
// db's staging generation (learnIndexes.of) where there is one, else (and
// always with reread) from db's reads. In the exact view's verify mode a group found through the index
// is compared with the one every candidate of a read gives, as is its owner
// of each recording of it, and a difference panics.
func (h *HTTP) learnedGroupOf(base, db integrations.MockMemDb, tracker *learn.Tracker, live httpParts, liveShape learn.Shape, n learnNoise, reread bool) ([]*models.Mock, func(*models.Mock) bool, error) {
	read := func() (scopeReads, error) { return h.learnReads(db, tracker) }
	var ix *learnIndex
	if !reread {
		ix = h.learnIx.of(base, db, n, func() ([]*models.Mock, []*models.Mock, error) {
			r, err := read()
			return r.perTest, r.session, err
		})
	}
	if ix == nil {
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
	r.sieve = n.learnSieveOf(live)
	sc := ix.scope(r, mockSpentOf(db), live, liveShape, n)
	group, owner := exactGroup(liveShape, sc, false), sc.owner()
	if verify {
		full, err := read()
		if err != nil {
			return nil, nil, err
		}
		// Another call's consume between the index's look and the read
		// drops a recording from the read alone, and a test's open or close
		// moves the generation: both are compared less what the store
		// reports spent after the read, and only while the generation the
		// index is of still holds.
		if again, _ := integrations.StagingGenerationOf(db, models.HTTP); again == ix.gen {
			full.answered, full.answeredMocks = r.answered, r.answeredMocks
			fsc := full.scope(live, liveShape, n, true)
			all, allOwner := exactGroup(liveShape, fsc, false), fsc.owner()
			spentNow := mockSpentOf(db)
			unspent := func(ms []*models.Mock) []*models.Mock {
				return slices.DeleteFunc(slices.Clone(ms), func(m *models.Mock) bool { return spentNow(m.Name) })
			}
			if !slices.Equal(unspent(all), unspent(group)) {
				panic(fmt.Sprintf("exact view verify: indexed learned group of %s %s: indexed %v, read %v", live.method, live.path, strictNames(group), strictNames(all)))
			}
			for _, m := range unspent(group) {
				if owner(m) != allOwner(m) {
					panic(fmt.Sprintf("exact view verify: indexed learned group of %s %s: %s is the open test's own %v through the index, %v from a read", live.method, live.path, m.Name, owner(m), allOwner(m)))
				}
			}
		}
	}
	return group, owner, nil
}

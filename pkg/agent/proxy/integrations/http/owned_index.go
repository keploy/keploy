package http

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// ownedIndexes keeps an HTTP parser's last index of an open test's own
// recordings (integrations.SharedReads' OwnedRecordings), for one store, one
// view and one url noise: the strict pass's (strictOwned) or the learn
// step's (learnOwned). A view's own recordings never change while it lasts,
// so a test's calls file them once, at its first, and each call then reads
// what is left of its bucket (OwnedUnansweredAt), never every recording the
// test has not answered: a test that owns many recordings costs a call what
// one that owns few does.
//
// An index holds positions in the view's list and keys made from the
// recordings' paths, never the recordings: each call reads the list of the
// view open now (OwnedRecordings), so a test's recordings go with its view,
// whatever the parser keeps until its next call.
type ownedIndexes[T any] struct {
	mu   sync.Mutex
	key  ownedIndexKey
	last T
	has  bool
}

// ownedIndexKey names what an ownedIndexes index is of: the store the reads
// are held from (base), the view whose own recordings it files, and the url
// noise their paths are masked with.
type ownedIndexKey struct {
	store integrations.MockMemDb
	view  uint64
	url   string
}

// of is the index of the open test's own recordings sr reads now, for base,
// with url the url noise its paths are masked with, built from the view's
// list where none is kept for it, the view's number, and its list, which the
// index's positions are of; false while no test is open. The view's list is
// the store's and is never changed here.
func (x *ownedIndexes[T]) of(base integrations.MockMemDb, sr integrations.SharedReads, url string, build func(owned []*models.Mock) T) (T, uint64, []*models.Mock, bool) {
	var none T
	view, owned, ok := sr.OwnedRecordings()
	if !ok {
		return none, 0, nil, false
	}
	key := ownedIndexKey{store: base, view: view, url: url}
	x.mu.Lock()
	if x.has && x.key == key {
		ix := x.last
		x.mu.Unlock()
		return ix, view, owned, true
	}
	// The kept index is of another test's view: let go of it, and of the
	// recordings it holds, now rather than at the next build.
	x.last, x.key, x.has = none, ownedIndexKey{}, false
	x.mu.Unlock()
	// And when base lets its recordings go (a staging, its Close), let go
	// of what is kept of base then, rather than at the next call, which may
	// come only in a later set, or never.
	integrations.WhenRestaged(base, x, func() { x.release(base) })
	ix := build(owned)
	x.mu.Lock()
	x.last, x.key, x.has = ix, key, true
	x.mu.Unlock()
	return ix, view, owned, true
}

// release lets go of what x keeps of store.
func (x *ownedIndexes[T]) release(store integrations.MockMemDb) {
	var none T
	x.mu.Lock()
	if x.has && x.key.store == store {
		x.last, x.key, x.has = none, ownedIndexKey{}, false
	}
	x.mu.Unlock()
}

// ownedLive is what is left of the positions at of the open test's view
// numbered view (OwnedUnansweredAt), and the staging generation they are of:
// false where that view is no longer the open test's, or the shared part
// moved from key's, when the call reads the store instead. The generation is
// sampled before the read, and the shared part after it, as the reads of an
// index of the shared part are (integrations.SharedReads).
func ownedLive(sr integrations.SharedReads, key learnIndexKey, view uint64, at []int) (uint64, []int, bool) {
	gen := sr.StagingGenerationByKind(models.HTTP)
	live, ok := sr.OwnedUnansweredAt(view, at)
	if !ok {
		return 0, nil, false
	}
	if again, ok := sr.SharedGeneration(models.HTTP); !ok || again != key.shared {
		return 0, nil, false
	}
	return gen, live, true
}

// strictOwned is the strict pass's index of an open test's own recordings
// (ownedIndexes): the positions in the view's list of the HTTP recordings
// that declare no variables and recorded a request, in the list's order,
// filed under their method and masked path (strictKey), those that learned a
// place of their URL under their method alone (learnsURL: a call of another
// path of the method may be them), and the positions of those that declare
// variables (vars), which the variable pass reads for every call.
type strictOwned struct {
	byKey map[string][]int
	byURL map[string][]int
	vars  []int
}

func buildStrictOwned(owned []*models.Mock, n strictNoise) *strictOwned {
	ix := &strictOwned{byKey: map[string][]int{}, byURL: map[string][]int{}}
	for p, m := range owned {
		if m == nil || m.Kind != models.HTTP {
			continue
		}
		if hasVariables(m) {
			ix.vars = append(ix.vars, p)
			continue
		}
		if m.Spec.HTTPReq == nil {
			continue
		}
		if learnsURL(m) {
			method := string(m.Spec.HTTPReq.Method)
			ix.byURL[method] = append(ix.byURL[method], p)
			continue
		}
		k := strictKey(m, n)
		ix.byKey[k] = append(ix.byKey[k], p)
	}
	return ix
}

// at is the positions a strict call of the sieve's method and masked path
// may be, in the list's order, then those that declare variables.
func (ix *strictOwned) at(s strictSieve) []int {
	exact, wide := ix.byKey[s.method+" "+s.masked], ix.byURL[s.method]
	out := make([]int, 0, len(exact)+len(wide)+len(ix.vars))
	out = append(append(out, exact...), wide...)
	if len(wide) > 0 && len(exact) > 0 {
		slices.Sort(out)
	}
	return append(out, ix.vars...)
}

// learnOwned is the learn step's index of an open test's own recordings
// (ownedIndexes): the HTTP recordings that declare no variables and recorded
// a request, in recorded order (inRecordedOrder), by rank: each one's
// position in the view's list (pos) and masked path (masked), filed under
// its method and masked path, and those a segment of whose path may be
// excused (learnsWide) under their method and segment count too, as
// buildLearnIndex files them.
type learnOwned struct {
	pos    []int
	masked []string
	byPath map[string][]int
	wild   map[string][]int
}

func buildLearnOwned(owned []*models.Mock, n learnNoise) *learnOwned {
	ix := &learnOwned{byPath: map[string][]int{}, wild: map[string][]int{}}
	for p, m := range owned {
		if m != nil && m.Kind == models.HTTP && m.Spec.HTTPReq != nil && !hasVariables(m) {
			ix.pos = append(ix.pos, p)
		}
	}
	// inRecordedOrder's order: a stable sort of the list's order.
	sort.SliceStable(ix.pos, func(i, j int) bool { return recordedFirst(owned[ix.pos[i]], owned[ix.pos[j]]) })
	for r, p := range ix.pos {
		m := owned[p]
		rp := recordedParts(m)
		// A copy: the masked path may be a part of the recording's URL,
		// which the index would otherwise keep.
		masked := strings.Clone(n.maskedPath(rp.path))
		ix.masked = append(ix.masked, masked)
		ix.byPath[rp.method+" "+masked] = append(ix.byPath[rp.method+" "+masked], r)
		if learnsWide(m) {
			w := rp.method + " " + strconv.Itoa(segmentCount(masked))
			ix.wild[w] = append(ix.wild[w], r)
		}
	}
	return ix
}

// ranks are the entries a call of method whose masked path is masked may be,
// in recorded order: those of its method and masked path, and those of its
// method and segment count a segment of whose path may be excused
// (learnIndex.positions' rule).
func (ix *learnOwned) ranks(method, masked string) []int {
	exact, wild := ix.byPath[method+" "+masked], ix.wild[method+" "+strconv.Itoa(segmentCount(masked))]
	if len(wild) == 0 {
		return exact
	}
	out := append(append(make([]int, 0, len(exact)+len(wild)), exact...), wild...)
	slices.Sort(out)
	return slices.Compact(out)
}

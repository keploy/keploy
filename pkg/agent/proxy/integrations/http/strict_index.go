package http

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// strictIndex is the strict pass's index of the HTTP recordings a store's
// reads gave at one staging generation (integrations.StagingGenerations):
// the pool matchCascade hands strictServe, in its order (the per-test read's
// HTTP recordings, then the session read's), filed under its method and its
// path with keploy.yml's url noise masked, as strictSieve reads them. A
// recording that learned a place of its URL (learnsURL) is filed under its
// method alone, since strictSieve lets a call of any path be it. So a strict
// call compares the recordings that may be it, not the whole staged set.
//
// While the generation holds, the store's reads are what they were when it
// began, less the mocks MockSpent reports (a consume under exact serving), so
// the index, less those and less what a test kept for its repeats and is
// over with (integrations.SpentReader), is the pool a read would give.
// While a test is open the index is of the shared part of its reads, beside
// the test's unanswered recordings read for each call (learnIndex,
// strictLook).
//
// A recording that declares variables is the variable pass's, which serves
// those before the strict pass: the index keeps them aside (vars), so a call
// runs the variable pass over them and the test's own, less what is spent,
// as it would over a read, and goes on to the strict pass's bucket only when
// none fits (strictIndexed).
type strictIndex struct {
	gen     uint64
	entries []*models.Mock
	// split: entries[:split] are the per-test read's, the rest the session
	// read's.
	split  int
	byPath map[string][]int
	wild   map[string][]int
	// vars are the reads' HTTP recordings that declare variables, in their
	// order: vars[:varSplit] the per-test read's, the rest the session
	// read's.
	vars     []*models.Mock
	varSplit int
}

// strictIndexBuilds counts the strict indexes built, and strictVisits the
// recordings a strict call looked at (strictSieve's walk, or an index's bucket): what
// the rows count to see that one generation builds one index and that a call
// reads only its bucket.
var (
	strictIndexBuilds atomic.Int64
	strictVisits      atomic.Int64
)

// buildStrictIndex files the strict pool of the reads perTest and session.
func buildStrictIndex(gen uint64, perTest, session []*models.Mock, n strictNoise) *strictIndex {
	strictIndexBuilds.Add(1)
	ix := &strictIndex{gen: gen, byPath: map[string][]int{}, wild: map[string][]int{}}
	for i, mocks := range [][]*models.Mock{perTest, session} {
		if i == 1 {
			ix.split, ix.varSplit = len(ix.entries), len(ix.vars)
		}
		for _, m := range FilterHTTPMocks(mocks) {
			if hasVariables(m) {
				ix.vars = append(ix.vars, m)
				continue
			}
			r := m.Spec.HTTPReq
			if r == nil {
				continue // strictSieve: a recording of no request is no call
			}
			p := len(ix.entries)
			ix.entries = append(ix.entries, m)
			if learnsURL(m) {
				ix.wild[string(r.Method)] = append(ix.wild[string(r.Method)], p)
				continue
			}
			k := strictKey(m, n)
			ix.byPath[k] = append(ix.byPath[k], p)
		}
	}
	return ix
}

// positions are the entries a call of the sieve's method and masked path may
// be (strictSieve.mayBe), in the pool's order.
func (ix *strictIndex) positions(s strictSieve) []int {
	exact, wild := ix.byPath[s.method+" "+s.masked], ix.wild[s.method]
	if len(wild) == 0 {
		return exact
	}
	out := append(append(make([]int, 0, len(exact)+len(wild)), exact...), wild...)
	slices.Sort(out)
	return out
}

// strictKey is what buildStrictIndex files m under, a recording that learned
// no place of its URL: its method and its path with the url noise masked.
func strictKey(m *models.Mock, n strictNoise) string {
	path, _ := recordedStrictURL(m)
	return string(m.Spec.HTTPReq.Method) + " " + strings.TrimPrefix(maskURLNoise(path, n.url), "/")
}

// strictIndexes keeps the last strictIndex an HTTP parser built
// (genIndexes): of a store's reads at one staging generation, and of the
// shared part of an open test's reads at one SharedGen
// (integrations.SharedReads).
type strictIndexes struct {
	genIndexes[*strictIndex]
	shared genIndexes[*strictIndex]
	// owned is the index of the open test's own recordings beside shared.
	owned ownedIndexes[*strictOwned]
}

// strictLook is the strict pass's indexed read for one call (learnLook's
// kind): an index, and for an index of the shared part of an open test's
// reads, the test's unanswered HTTP recordings the call of the sieve's
// method and path may be, read for the call through the index of the test's
// own (strictOwned), in their order, which lead each read (owned);
// ownedVars are the test's unanswered ones that declare variables
// (strictIndex.vars). gen is the staging generation the read is of.
type strictLook struct {
	ix        *strictIndex
	shared    bool
	owned     []*models.Mock
	ownedVars []*models.Mock
	gen       uint64
}

// of is the strict pass's indexed read of what db reads now, for base, for a
// call of the sieve's method and path, or false where the call reads the
// store instead (learnIndexes.of's rule).
func (x *strictIndexes) of(base, db integrations.MockMemDb, n strictNoise, s strictSieve) (strictLook, bool) {
	url := urlNoiseKey(n.url)
	if sr, ok := db.(integrations.SharedReads); ok {
		var perTest, session []*models.Mock
		ix, key, ok := x.shared.sharedOf(base, db, url, func() error {
			var ok bool
			if perTest, session, ok = sr.SharedMocks(); !ok {
				return errNoSharedReads
			}
			return nil
		}, func(key learnIndexKey) *strictIndex {
			return buildStrictIndex(key.gen, perTest, session, n)
		})
		if ok {
			own, view, owned, ok := x.owned.of(base, sr, url, func(owned []*models.Mock) *strictOwned { return buildStrictOwned(owned, n) })
			if !ok {
				return strictLook{}, false
			}
			gen, live, ok := ownedLive(sr, key, view, own.at(s))
			if !ok {
				return strictLook{}, false
			}
			look := strictLook{ix: ix, shared: true, gen: gen}
			for _, p := range live {
				if m := owned[p]; hasVariables(m) {
					look.ownedVars = append(look.ownedVars, m)
				} else {
					look.owned = append(look.owned, m)
				}
			}
			return look, true
		}
	}
	var perTest, session []*models.Mock
	ix, ok := x.genIndexes.of(base, db, url, func() error {
		var err error
		perTest, session, _, err = strictPool(db)
		return err
	}, func(key learnIndexKey) *strictIndex {
		return buildStrictIndex(key.gen, perTest, session, n)
	})
	if !ok {
		return strictLook{}, false
	}
	return strictLook{ix: ix, gen: ix.gen}, true
}

// variables are the recordings the variable pass reads for a call, as
// splitVariableMocks orders them out of a read's pool (the per-test read,
// then the session read, the test's own leading each, as candidates reads
// them), less what spent reports and, of the index, what mockSpent reports
// the store dropped since the generation began.
func (l strictLook) variables(mockSpent func(string) bool, spent func(*models.Mock) bool) []*models.Mock {
	if len(l.ix.vars) == 0 && len(l.ownedVars) == 0 {
		return nil
	}
	pool := make([]*models.Mock, 0, 2*len(l.ownedVars)+len(l.ix.vars))
	for _, part := range [][]*models.Mock{l.ix.vars[:l.ix.varSplit], l.ix.vars[l.ix.varSplit:]} {
		for _, m := range l.ownedVars {
			if !spent(m) {
				pool = append(pool, m)
			}
		}
		for _, m := range part {
			if !mockSpent(m.Name) && !spent(m) {
				pool = append(pool, m)
			}
		}
	}
	withVars, _ := splitVariableMocks(pool)
	return withVars
}

// candidates are the recordings a call of the sieve's method and masked path
// may be, in the pool's order (strictPool), less what spent reports (a test
// kept them for its repeats and is over with) and, of the index, what
// mockSpent reports the store dropped since the generation began. Of an index
// of an open test's shared part the pool is the per-test read (the test's
// unanswered recordings, then the tier's shared part) then the session read
// (the test's unanswered recordings, then the tier's shared part less those).
func (l strictLook) candidates(s strictSieve, n strictNoise, mockSpent func(string) bool, spent func(*models.Mock) bool) []*models.Mock {
	ps := l.ix.positions(s)
	var owned []*models.Mock
	if l.shared {
		// l.owned is the test's own the call may be already (strictOwned.at).
		for _, m := range l.owned {
			if !spent(m) {
				owned = append(owned, m)
			}
		}
	}
	out := make([]*models.Mock, 0, 2*len(owned)+len(ps))
	i := 0
	for _, perTest := range []bool{true, false} {
		out = append(out, owned...)
		for ; i < len(ps) && (ps[i] < l.ix.split) == perTest; i++ {
			m := l.ix.entries[ps[i]]
			if mockSpent(m.Name) || spent(m) {
				continue
			}
			out = append(out, m)
		}
	}
	return out
}

// spentReaderOf is db's account of the recordings a test kept for its
// repeats and is over with (integrations.WithoutSpent), or none.
func spentReaderOf(db integrations.MockMemDb) func(*models.Mock) bool {
	if sr, ok := db.(integrations.SpentReader); ok {
		return sr.Spent
	}
	return func(*models.Mock) bool { return false }
}

// strictPool is the pool matchCascade's strict pass reads: the reads' HTTP
// recordings, less what a test kept for its repeats and is over with, the
// per-test read's first.
func strictPool(db integrations.MockMemDb) (perTest, session, pool []*models.Mock, err error) {
	if perTest, err = db.GetPerTestMocksInWindow(); err != nil {
		return nil, nil, nil, err
	}
	//nolint:staticcheck // SA1019: the strict pass's own read (matchCascade), whose pool the index must be.
	if session, err = db.GetSessionMocks(); err != nil {
		return nil, nil, nil, err
	}
	combined := make([]*models.Mock, 0, len(perTest)+len(session))
	combined = append(append(combined, perTest...), session...)
	return perTest, session, FilterHTTPMocks(integrations.WithoutSpent(db, combined)), nil
}

// strictIndexed answers, under the proven switch, a call the strict pass
// finds in its bucket of h's index of db's staging generation, for base (the
// store db is held from: genIndexes.of), as strictServe would from a read.
// served nil and retry false is no answer here: there is no index, the store
// does not tell whose a recording is (integrations.TestOwner: every store
// that serves under the switch does), or the call is a miss or a stateful
// walk's miss, which a read decides (and names the closest recording of:
// strictServe).
//
// The recordings that declare variables (strictLook.variables) go through
// the variable pass first, as matchCascade's read does: a hit is served as
// there, and an answered stateful walk is the read's to report (no answer
// here). A call no recording with variables answers goes on to the strict
// pass's bucket, which a call whose variables did not hold in the recording
// it fit may still find (exactOnly's comparison). scope is read before the
// index, as matchCascade reads it before the pool.
//
// retry is true when another connection took the recording first, or a
// variable's bind met another's; the caller then reads the store. In the
// exact view's verify mode the fits found through the index, and the
// recordings the variable pass read, are compared with those of a read while
// the generation holds, and a difference panics.
func (h *HTTP) strictIndexed(ctx context.Context, input *req, db, base integrations.MockMemDb, n strictNoise, headerNoise, userBodyNoise map[string][]string, urlNoise []string, statefulMocks bool) (served *models.Mock, retry bool, err error) {
	owner, tells := db.(integrations.TestOwner)
	if !tells {
		return nil, false, nil
	}
	scope := readVarScope(db)
	sieve := newStrictSieve(input, n)
	look, ok := h.strictIx.of(base, db, n, sieve)
	if !ok {
		return nil, false, nil
	}
	own := owner.OwnedByOpenTest
	ctx = context.WithValue(ctx, ownedKey{}, own)
	mockSpent, spent := mockSpentOf(db), spentReaderOf(db)
	withVars := look.variables(mockSpent, spent)
	if integrations.ExactViewVerify.Load() {
		if err := verifyIndexedVariables(db, look.gen, withVars); err != nil {
			return nil, false, err
		}
	}
	if len(withVars) > 0 {
		// A recording that fits the call but whose variables do not hold
		// (the pass's problem) leaves the call to the strict pass alone, as
		// exactOnly does after a read: the bucket below is that comparison.
		hits, _, err := h.variablePass(ctx, input, withVars, scope, headerNoise, userBodyNoise, urlNoise)
		if err != nil {
			return nil, false, err
		}
		if len(hits) > 0 {
			hit, commitCursor, answered := h.pickHit(hits, own, db, statefulMocks, h.variableMember(ctx, input, scope, headerNoise, userBodyNoise, urlNoise))
			if answered != nil {
				return nil, false, nil
			}
			served, problem, retry, err := h.serveVariables(ctx, hit, db)
			if err != nil || retry || problem != nil {
				return nil, retry, err
			}
			if commitCursor != nil {
				commitCursor()
			}
			return served, false, nil
		}
	}
	live := liveDestination(ctx, input)
	cands := look.candidates(sieve, n, mockSpent, spent)
	strictVisits.Add(int64(len(cands)))
	var fits []*models.Mock
	for _, m := range cands {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if len(strictDiff(ctx, input, live, m, n)) == 0 {
			fits = append(fits, m)
		}
	}
	if integrations.ExactViewVerify.Load() {
		if err := verifyStrictIndexed(ctx, input, live, db, look.gen, fits, n); err != nil {
			return nil, false, err
		}
	}
	if len(fits) == 0 {
		return nil, false, nil
	}
	served, answered, retry, err := h.strictServeFits(ctx, input, live, fits, own, db, n, statefulMocks)
	if answered != nil {
		return nil, false, err
	}
	return served, retry, err
}

// verifyIndexedVariables compares the recordings the variable pass reads for
// a call through an indexed read of staging generation gen (withVars) with
// those it reads from a read's pool (splitVariableMocks), both less what the
// store reports spent after the read (strictUnspentOnce), while gen still holds,
// and panics on a difference.
func verifyIndexedVariables(db integrations.MockMemDb, gen uint64, withVars []*models.Mock) error {
	_, _, pool, err := strictPool(db)
	if err != nil {
		return err
	}
	read, _ := splitVariableMocks(pool)
	unspent := strictUnspentOnce(db)
	got, want := unspent(withVars), unspent(read)
	if again, _ := integrations.StagingGenerationOf(db, models.HTTP); again != gen {
		return nil
	}
	if !slices.Equal(got, want) {
		panic(fmt.Sprintf("exact view verify: indexed variable recordings: indexed %v, read %v", strictNames(got), strictNames(want)))
	}
	return nil
}

// verifyStrictIndexed compares the fits a strict call found through an
// indexed read of staging generation gen with those of a read's whole pool
// (strictCompare), both less what the store reports spent after the read
// (strictUnspentOnce), while gen still holds (a test's open or close moves it),
// and panics on a difference.
func verifyStrictIndexed(ctx context.Context, input *req, live destination, db integrations.MockMemDb, gen uint64, fits []*models.Mock, n strictNoise) error {
	_, _, pool, err := strictPool(db)
	if err != nil {
		return err
	}
	_, pool = splitVariableMocks(pool)
	all, _, err := strictCompare(ctx, input, live, pool, n)
	if err != nil {
		return err
	}
	unspent := strictUnspentOnce(db)
	got, want := unspent(fits), unspent(all)
	if again, _ := integrations.StagingGenerationOf(db, models.HTTP); again != gen {
		return nil
	}
	if !slices.Equal(got, want) {
		panic(fmt.Sprintf("exact view verify: indexed strict fits of %s %s: indexed %v, read %v", input.method, newStrictSieve(input, n).path, strictNames(got), strictNames(want)))
	}
	return nil
}

// strictUnspentOnce is unspentOnce of what the strict pass's reads drop: what
// db reports spent (MockSpent) and what a test kept for its repeats and is
// over with (integrations.SpentReader).
func strictUnspentOnce(db integrations.MockMemDb) func([]*models.Mock) []*models.Mock {
	mockSpent, spent := mockSpentOf(db), spentReaderOf(db)
	return unspentOnce(func(m *models.Mock) bool { return mockSpent(m.Name) || spent(m) })
}

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
type strictIndex struct {
	gen     uint64
	entries []*models.Mock
	byPath  map[string][]int
	wild    map[string][]int
	// vars: a read held a recording that declares variables. The variable
	// pass serves those before the strict pass, from the whole read, so no
	// call of the generation is served through the index.
	vars bool
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
	for _, mocks := range [][]*models.Mock{perTest, session} {
		for _, m := range FilterHTTPMocks(mocks) {
			if hasVariables(m) {
				ix.vars = true
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
			path, _ := recordedStrictURL(m)
			k := string(r.Method) + " " + strings.TrimPrefix(maskURLNoise(path, n.url), "/")
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
// that serves under the switch does), the generation holds a recording that
// declares variables, or the call is a miss or a stateful walk's miss, which
// a read decides (and names the closest recording of: strictServe). retry is true when another connection
// took the recording first; the caller then reads the store. In the exact
// view's verify mode the fits found through the index are compared with
// those of a read while the generation holds, and a difference panics.
func (h *HTTP) strictIndexed(ctx context.Context, input *req, db, base integrations.MockMemDb, n strictNoise, statefulMocks bool) (served *models.Mock, retry bool, err error) {
	owner, tells := db.(integrations.TestOwner)
	if !tells {
		return nil, false, nil
	}
	var perTest, session []*models.Mock
	ix, ok := h.strictIx.of(base, db, urlNoiseKey(n.url), func() error {
		var err error
		perTest, session, _, err = strictPool(db)
		return err
	}, func(key learnIndexKey) *strictIndex {
		return buildStrictIndex(key.gen, perTest, session, n)
	})
	if !ok || ix.vars {
		return nil, false, nil
	}
	live := liveDestination(ctx, input)
	sieve := newStrictSieve(input, n)
	mockSpent, spent := mockSpentOf(db), spentReaderOf(db)
	positions := ix.positions(sieve)
	strictVisits.Add(int64(len(positions)))
	var fits []*models.Mock
	for _, p := range positions {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		m := ix.entries[p]
		if mockSpent(m.Name) || spent(m) {
			continue
		}
		if len(strictDiff(ctx, input, live, m, n)) == 0 {
			fits = append(fits, m)
		}
	}
	if integrations.ExactViewVerify.Load() {
		if err := verifyStrictIndexed(ctx, input, live, db, ix, fits, n); err != nil {
			return nil, false, err
		}
	}
	if len(fits) == 0 {
		return nil, false, nil
	}
	own := owner.OwnedByOpenTest
	served, answered, retry, err := h.strictServeFits(context.WithValue(ctx, ownedKey{}, own), input, live, fits, own, db, n, statefulMocks)
	if answered != nil {
		return nil, false, err
	}
	return served, retry, err
}

// verifyStrictIndexed compares the fits a strict call found through ix with
// those of a read's whole pool (strictCompare), both less what the store
// reports spent after the read, while ix's generation still holds (a test's
// open or close moves it), and panics on a difference.
func verifyStrictIndexed(ctx context.Context, input *req, live destination, db integrations.MockMemDb, ix *strictIndex, fits []*models.Mock, n strictNoise) error {
	_, _, pool, err := strictPool(db)
	if err != nil {
		return err
	}
	if again, _ := integrations.StagingGenerationOf(db, models.HTTP); again != ix.gen {
		return nil
	}
	_, pool = splitVariableMocks(pool)
	all, _, err := strictCompare(ctx, input, live, pool, n)
	if err != nil {
		return err
	}
	mockSpent, spent := mockSpentOf(db), spentReaderOf(db)
	unspent := func(ms []*models.Mock) []*models.Mock {
		return slices.DeleteFunc(slices.Clone(ms), func(m *models.Mock) bool { return mockSpent(m.Name) || spent(m) })
	}
	if got, want := unspent(fits), unspent(all); !slices.Equal(got, want) {
		panic(fmt.Sprintf("exact view verify: indexed strict fits of %s %s: indexed %v, read %v", input.method, newStrictSieve(input, n).path, strictNames(got), strictNames(want)))
	}
	return nil
}

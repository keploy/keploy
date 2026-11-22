package http

import (
	"context"
	"encoding/binary"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// strictKeyIndexes holds an index for each url noise a strict call read
// through (strictKeyIndexOf), by its patterns, at most noiseIndexesCap of
// them.
var strictKeyIndexes struct {
	mu sync.Mutex
	m  map[string]*integrations.MockIndex
}

// strictKeyIndexOf is the index that files each HTTP recording, under n's
// url noise, as a call under the proven switch with no exact view reads it
// (keyedStrictRead), and as buildStrictIndex files a staging generation:
//   - one that declares variables under strictVarsKey: the variable pass
//     reads every one of them;
//   - one that learned a place of its URL (learnsURL) under its method alone
//     (strictWildKey), since strictSieve lets a call of any path be it;
//   - any other under its method and its path with the url noise masked
//     (strictKey) as strictPathKey, and, by its body, under its print
//     (plainPrint: strictPrintKey), or under strictUnprintedKey where the
//     strict pass compares its body otherwise;
//   - each of the last two kinds under its method too (strictMethodKey).
//
// A recording of no request is filed under none of the last three: it is no
// call (strictSieve).
func strictKeyIndexOf(n strictNoise) *integrations.MockIndex {
	cfg := urlNoiseKey(n.url)
	strictKeyIndexes.mu.Lock()
	defer strictKeyIndexes.mu.Unlock()
	if ix := strictKeyIndexes.m[cfg]; ix != nil {
		return ix
	}
	if strictKeyIndexes.m == nil || len(strictKeyIndexes.m) >= noiseIndexesCap {
		strictKeyIndexes.m = map[string]*integrations.MockIndex{}
	}
	url := strictNoise{url: n.url}
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string { return strictKeys(m, url) }}
	strictKeyIndexes.m[cfg] = ix
	return ix
}

// strictKeys are the keys strictKeyIndexOf files m under, url noise n.
func strictKeys(m *models.Mock, n strictNoise) []string {
	if m == nil || m.Kind != models.Kind(models.HTTP) {
		return nil
	}
	if hasVariables(m) {
		return []string{strictVarsKey}
	}
	r := m.Spec.HTTPReq
	if r == nil {
		return nil
	}
	method := string(r.Method)
	if learnsURL(m) {
		return []string{strictMethodKey(method), strictWildKey(method)}
	}
	k := strictKey(m, n)
	body := strictUnprintedKey(k)
	if sum, ok := plainPrint(m); ok {
		body = strictPrintKey(k, sum)
	}
	return []string{strictMethodKey(method), strictPathKey(k), body}
}

// The keys of strictKeyIndexOf: each kind begins with a letter of its own,
// and a method holds no space, so no two kinds, methods or paths share one.
const strictVarsKey = "v"

func strictWildKey(method string) string   { return "w" + method }
func strictMethodKey(method string) string { return "m" + method }

// strictPathKey is the key of the recordings of the strict key k (strictKey:
// a method and a masked path).
func strictPathKey(k string) string { return "s" + k }

// strictUnprintedKey is the key of the recordings of the strict key k whose
// body the strict pass compares otherwise than by its print (plainPrint).
func strictUnprintedKey(k string) string { return "u" + k }

// strictPrintKey is the key of the recordings of the strict key k whose body
// has the print sum. The print is its last 8 bytes, so two keys are one only
// where both k and sum are.
func strictPrintKey(k string, sum uint64) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], sum)
	return "b" + k + string(b[:])
}

// keyedStrictRead answers a call under the proven switch with no exact view,
// through the stores' key indexes (strictKeyIndexOf), as matchCascade's read
// of the whole pool answers it, the recordings that declare variables
// through the variable pass, then the rest through the strict pass:
//   - the variable pass reads every recording that declares variables, the
//     per-test ones, then the session ones, less what is spent, as
//     splitVariableMocks orders them out of the read's pool, so it finds the
//     hits it finds there and serves the one it serves there;
//   - the strict pass reads, of each tier, the recordings strictSieve lets
//     be the call: of the call's method and masked path, and of those, where
//     the call's body has a print (livePrint), only the ones of that print
//     and those whose body it compares otherwise (bodyBucket.of),
//     and those of its method that learned a place of their URL. Each list
//     is in the tier's order, and fits are compared on each recording alone,
//     so the fits of one list are the read's fits of that tier in order;
//     where two lists of a tier hold fits, they are put in the order of the
//     tier's list of the call's method, which holds both (strictFitsInOrder).
//
// Its one caller, matchCascade, calls it under the switch alone. served nil
// and retry false is no answer here, and the call reads the whole pool: an
// exact view or a stateful sequence view is in force, the store reads no
// key, or any outcome but an answer (a miss, a recording with variables that
// fits the call but whose variables do not hold, an answered stateful walk),
// which the read decides and reports. So each call is answered, missed and
// reported as the whole read does it.
// retry is true when another connection took the recording first, or a
// variable's bind met another's, as the read retries then.
func (h *HTTP) keyedStrictRead(ctx context.Context, input *req, mockDb integrations.MockMemDb, scope varScope, n strictNoise, headerNoise, userBodyNoise map[string][]string, urlNoise []string, statefulMocks bool) (served *models.Mock, retry bool, err error) {
	if integrations.ExactServing(mockDb) {
		return nil, false, nil
	}
	if _, exact := integrations.SequenceViewOf(mockDb); exact {
		return nil, false, nil
	}
	pr, ok := mockDb.(integrations.PerTestKeyRanger)
	if !ok {
		return nil, false, nil
	}
	sr, ok := mockDb.(integrations.SessionKeyReader)
	if !ok {
		return nil, false, nil
	}
	ix := strictKeyIndexOf(n)
	// read is, of each tier, the recordings ix files under key, in its
	// order, less what is spent.
	read := func(key string) (tiers [2][]*models.Mock, ok bool) {
		var keyed []*models.Mock
		add := func(mk *models.Mock) bool {
			keyed = append(keyed, mk)
			return true
		}
		if err := pr.RangePerTestMocksWithKey(ix, key, add); err != nil {
			utils.LogError(h.Logger, err, "failed to get per-test mocks")
			return tiers, false
		}
		split := len(keyed)
		if err := sr.RangeSessionMocksWithKey(ix, key, add); err != nil {
			utils.LogError(h.Logger, err, "failed to get session mocks")
			return tiers, false
		}
		tiers[0] = FilterHTTPMocks(integrations.WithoutSpent(mockDb, keyed[:split:split]))
		tiers[1] = FilterHTTPMocks(integrations.WithoutSpent(mockDb, keyed[split:]))
		return tiers, true
	}
	sieve := newStrictSieve(input, n)
	key := sieve.method + " " + sieve.masked
	keys := []string{strictPathKey(key)}
	if sum, printed := livePrint(mockDb, n, input.body); printed {
		keys = []string{strictPrintKey(key, sum), strictUnprintedKey(key)}
	}
	keys = append(keys, strictWildKey(sieve.method))
	vars, ok := read(strictVarsKey)
	if !ok {
		return nil, false, nil
	}
	perTest := vars[0]
	lists := make([][2][]*models.Mock, 0, len(keys))
	for _, k := range keys {
		l, ok := read(k)
		if !ok {
			return nil, false, nil
		}
		lists = append(lists, l)
		perTest = append(perTest[:len(perTest):len(perTest)], l[0]...)
	}
	own := openTestOwner(mockDb, perTest)
	ctx = context.WithValue(context.WithValue(ctx, ownedKey{}, own), storeKey{}, mockDb)

	if withVars, _ := splitVariableMocks(append(vars[0][:len(vars[0]):len(vars[0])], vars[1]...)); len(withVars) > 0 {
		hits, problem, err := h.variablePass(ctx, input, withVars, scope, headerNoise, userBodyNoise, urlNoise)
		if err != nil {
			return nil, false, err
		}
		if len(hits) > 0 {
			hit, commitCursor, answered := h.pickHit(hits, own, mockDb, statefulMocks, h.variableMember(ctx, input, scope, headerNoise, userBodyNoise, urlNoise))
			if answered != nil {
				return nil, false, nil
			}
			// The keyed read read no pool: the debug line of a bind reads
			// the store's (storePlaces).
			served, problem, retry, err := h.serveVariables(ctx, hit, mockDb, func() redactedPlaces {
				return storePlaces(mockDb, hit.mock, input.url.EscapedPath())
			})
			if err != nil || retry || problem != nil {
				return nil, retry, err
			}
			if commitCursor != nil {
				commitCursor()
			}
			return served, false, nil
		}
		if problem != nil {
			// Only an exact recording of this very call may still answer
			// it (exactOnly): the read decides it.
			return nil, false, nil
		}
	}

	live := liveDestination(ctx, input)
	var fits []*models.Mock
	for t := range 2 {
		var of [][]*models.Mock
		for _, l := range lists {
			f, err := strictFits(ctx, input, live, l[t], n, h.verifying())
			if err != nil {
				return nil, false, err
			}
			if len(f) > 0 {
				of = append(of, f)
			}
		}
		switch len(of) {
		case 0:
		case 1:
			fits = append(fits, of[0]...)
		default:
			ordered, ok := strictFitsInOrder(pr, sr, ix, t, sieve.method, of)
			if !ok {
				return nil, false, nil
			}
			fits = append(fits, ordered...)
		}
	}
	if len(fits) == 0 {
		return nil, false, nil
	}
	served, answered, retry, err := h.strictServeFits(ctx, input, live, fits, own, mockDb, n, statefulMocks)
	if answered != nil {
		return nil, false, nil
	}
	return served, retry, err
}

// strictFitsInOrder are the fits of, found in several lists of tier t (0
// the per-test tier, 1 the session one), in the tier's order: that of the
// tier's list of method (strictMethodKey), which files every recording of
// those lists. ok is false where that list cannot be read, or no longer holds
// every one of them (the store changed since): the call reads the whole pool.
func strictFitsInOrder(pr integrations.PerTestKeyRanger, sr integrations.SessionKeyReader, ix *integrations.MockIndex, t int, method string, of [][]*models.Mock) ([]*models.Mock, bool) {
	want := map[*models.Mock]bool{}
	for _, f := range of {
		for _, m := range f {
			want[m] = true
		}
	}
	need := len(want)
	out := make([]*models.Mock, 0, need)
	keep := func(mk *models.Mock) bool {
		if want[mk] {
			delete(want, mk)
			out = append(out, mk)
		}
		return len(out) < need
	}
	var err error
	if t == 0 {
		err = pr.RangePerTestMocksWithKey(ix, strictMethodKey(method), keep)
	} else {
		err = sr.RangeSessionMocksWithKey(ix, strictMethodKey(method), keep)
	}
	return out, err == nil && len(out) == need
}

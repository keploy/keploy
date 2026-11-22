package http

import (
	"context"
	"encoding/binary"
	"hash/maphash"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// requestIndex files each HTTP recording under its request: its method, its
// recorded path and its body (requestKey). With the proven switch off, the
// exact-body pass of a call (keyedExactBody) reads the recordings filed under
// the call's own request, not the whole pool, so a call costs the same in a
// set of ten recordings or of ten thousand.
var requestIndex = &integrations.MockIndex{Keys: requestKeys}

// requestSeed seeds requestKey's body hash. Keys live as long as the process,
// as the stores' indexes do.
var requestSeed = maphash.MakeSeed()

// requestKeys is requestIndex's key of m: none for a mock that is not an HTTP
// recording, or whose URL does not parse, which no call's path matches
// (MatchURLPath).
func requestKeys(m *models.Mock) []string {
	if m == nil || m.Kind != models.Kind(models.HTTP) || m.Spec.HTTPReq == nil {
		return nil
	}
	path, ok := recordedURLPath(m.Spec.HTTPReq.URL)
	if !ok {
		return nil
	}
	return []string{requestKey(string(m.Spec.HTTPReq.Method), path, maphash.String(requestSeed, m.Spec.HTTPReq.Body))}
}

// requestKey is the key of a request by its method, its path and its body's
// hash. A method holds no space and the hash is the last 8 bytes, so no two
// requests that differ in any of the three share a key unless their bodies'
// hashes do; a reader compares the bodies themselves.
func requestKey(method, path string, body uint64) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], body)
	return method + " " + path + "\x00" + string(b[:])
}

// keyedHit is what a call's keyed exact-body pass (keyedExactBody) did.
type keyedHit struct {
	// ctx is the call's context from the pass on, and end, when not nil,
	// gives back the turn the pass took (admit).
	ctx context.Context
	end func()
	// found: a recording of the call's very request answers it. served is
	// what the claim served, nil when another call took the recording
	// first: the call then reads the store again.
	found  bool
	served *models.Mock
}

// keyedExactBody is matchCascade's first answer with the proven switch off,
// its exact-body pass, read through requestIndex instead of the whole pool:
// the per-test recordings, then the session ones, that the store files under
// the call's method, path and body hash, in the pool's order, less what is
// spent, through the same schema match. That pass serves the pool's first
// schema-matched recording whose body is the call's, byte for byte; such a
// recording has the call's method, path and body and so its key, and the
// schema match keeps or drops each recording on its own, so it is the first
// of these too. The stateful cursor's group (cursorGroup) is the recordings of
// that one method, URL and body, all filed under the key, in the same order,
// so the cursor serves the same one of them. The claim is the pass's.
//
// found is false, and the cascade reads the whole pool as before, when no
// recording filed under the key is byte-equal to the call; when the call's
// url noise lets another path match; under exact serving, which reads by the
// test's mapping; or when the store reads no key. So each call is answered as
// the whole read answers it. The pass takes a turn (admit) only when the
// recordings it reads weigh over the bound, which the whole read then also
// does; mayAdmit is false once the call holds one.
func (h *HTTP) keyedExactBody(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise map[string][]string, urlNoise []string, statefulMocks, mayAdmit bool) (keyedHit, error) {
	hit := keyedHit{ctx: ctx}
	if provenswitch.On() || len(urlNoise) > 0 || integrations.ExactServing(mockDb) {
		return hit, nil
	}
	if _, exact := integrations.SequenceViewOf(mockDb); exact {
		return hit, nil
	}
	pr, ok := mockDb.(integrations.PerTestKeyRanger)
	if !ok {
		return hit, nil
	}
	sr, ok := mockDb.(integrations.SessionKeyReader)
	if !ok {
		return hit, nil
	}
	key := requestKey(input.method, input.url.Path, maphash.Bytes(requestSeed, input.body))
	var keyed []*models.Mock
	add := func(mk *models.Mock) bool {
		keyed = append(keyed, mk)
		return true
	}
	// A read that fails is the whole read's to report.
	if err := pr.RangePerTestMocksWithKey(requestIndex, key, add); err != nil {
		return hit, nil
	}
	perTest := keyed[:len(keyed):len(keyed)]
	if err := sr.RangeSessionMocksWithKey(requestIndex, key, add); err != nil {
		return hit, nil
	}
	pool := FilterHTTPMocks(integrations.WithoutSpent(mockDb, keyed))
	if len(pool) == 0 {
		return hit, nil
	}
	own := openTestOwner(mockDb, perTest)
	ctx = context.WithValue(ctx, ownedKey{}, own)
	ctx = context.WithValue(ctx, storeKey{}, mockDb)
	if weight := int64(len(input.body)) + recordedBytes(pool, models.Method(input.method)); mayAdmit && weight > heavyCallBytes {
		turn, end, err := admit(ctx, input.client, weight)
		if err != nil {
			return hit, err
		}
		ctx, hit.ctx, hit.end = turn, turn, end
	}
	schemaMatched, err := h.SchemaMatch(ctx, input, pool, headerNoise, nil, false)
	if err != nil {
		return hit, err
	}
	best := h.byteEqualBody(input.body, schemaMatched)
	if best == nil {
		return hit, nil
	}
	best, commitCursor, answered := h.cursorPick(best, schemaMatched, own, mockDb, statefulMocks)
	if answered != nil {
		return hit, nil
	}
	if ce := h.Logger.Check(zap.DebugLevel, "exact body match found"); ce != nil {
		ce.Write(zap.String("mock name", best.Name))
	}
	served, claimed, err := h.claim(context.WithValue(ctx, keepForRepeatsKey{}, true), best, mockDb, nil, nil)
	if err != nil {
		return hit, err
	}
	hit.found = true
	if !claimed {
		return hit, nil
	}
	if commitCursor != nil {
		commitCursor()
	}
	hit.served = served
	return hit, nil
}

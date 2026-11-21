package http

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"hash/fnv"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// strictBodies is one bucket of a strictIndex (its byPath entry) filed by
// the bodies of its recordings: the positions of the recordings whose body
// compares plainly (plainPrint) by their body's print, and the positions of
// the others (rest), each in the pool's order. A call whose body has a print
// may be only the recordings of that print and the rest: of a recording that
// compares its body plainly, strictDiff finds no difference in the body only
// where the call's body has the recording's print (bodyPrint).
type strictBodies struct {
	byPrint map[uint64][]int
	rest    []int
}

// strictBodyFilings counts the buckets filed by their recordings' bodies: what
// the rows count to see that a bucket is filed once, on its first call.
var strictBodyFilings atomic.Int64

// bodiesOf is the bucket key of ix filed by its recordings' bodies, filed on
// the first call that reads it so and kept for the index's life: the index's
// recordings are those of one staging generation, which a recording's change
// moves. One call files at a time (bodyTurn), each recording's body in turn,
// so a filing holds one decoded body; a call whose context ends while it
// waits or files leaves with its error, and the bucket unfiled.
func (ix *strictIndex) bodiesOf(ctx context.Context, key string) (*strictBodies, error) {
	select {
	case ix.bodyTurn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-ix.bodyTurn }()
	if b, ok := ix.bodies[key]; ok {
		return b, nil
	}
	b := &strictBodies{byPrint: map[uint64][]int{}}
	for _, p := range ix.byPath[key] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if sum, ok := plainPrint(ix.entries[p]); ok {
			b.byPrint[sum] = append(b.byPrint[sum], p)
		} else {
			b.rest = append(b.rest, p)
		}
	}
	strictBodyFilings.Add(1)
	ix.bodies[key] = b
	return b, nil
}

// of are the positions of the bucket a call whose body has the print sum may
// be, in the pool's order.
func (b *strictBodies) of(sum uint64) []int {
	same := b.byPrint[sum]
	if len(b.rest) == 0 {
		return same
	}
	if len(same) == 0 {
		return b.rest
	}
	out := append(append(make([]int, 0, len(same)+len(b.rest)), same...), b.rest...)
	slices.Sort(out)
	return out
}

// livePrint is the print of a strict call's body where its bucket is read by
// its recordings' bodies (bodiesOf), ok false where the call compares every
// recording of its bucket: where keploy.yml declares noise of request bodies
// (an entry excuses a member only the live body has, CoversAdded, or a value
// its patterns cover), where the replay may declare a source of a recording
// (a declared clock rule lets a field of its body change, clocksWith; the
// strict pass runs under the proven switch, which reads them), and where the
// body holds a number keploy cannot read.
func livePrint(db integrations.MockMemDb, n strictNoise, body []byte) (uint64, bool) {
	if !n.body.Empty() || integrations.DeclaresSources(db) {
		return 0, false
	}
	return bodyPrint(body, nil)
}

// plainPrint is the print of m's recorded body (bodyPrint), ok false where
// strictDiff may find no difference in a body of another print: where m
// learned noise of its body or a read of the clock in it (learnedAt), where
// its recorded body is a form or a multipart one (compared key by key and part
// by part), or where its body, or a value in it, is one the obfuscator
// redacted (compared with no live value).
func plainPrint(m *models.Mock) (uint64, bool) {
	r := m.Spec.HTTPReq
	if len(m.Spec.ReqBodyNoise) > 0 || bodyClock(m.Spec.Clocks) {
		return 0, false
	}
	switch mediaTypeKey(headerValue(r.Header, "Content-Type")) {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		return 0, false
	}
	k := strictKnown{values: util.NewNoiseChecker(m.Noise)}
	if k.redacted(r.Body) {
		return 0, false
	}
	return bodyPrint([]byte(r.Body), k.redacted)
}

// bodyClock reports whether clocks name a place of the request body (the
// learn step's dialect: body, body.items[0].at).
func bodyClock(clocks map[string]models.ClockOffset) bool {
	for p := range clocks {
		if strings.HasPrefix(p, "body") {
			return true
		}
	}
	return false
}

// bodyPrint is the print of a request body: of its JSON document where it is
// one (decodeJSONDoc), of its bytes otherwise. Two bodies bodyDiffs finds no
// difference in, for a recording whose body compares plainly (plainPrint),
// have one print: bodyDiffs passes equal bytes, which decode alike, and two
// documents only where they hold the same members, the same elements in the
// same order and scalars sameScalar holds the same (jsonDiffs with no noise,
// nothing learned and nothing redacted), which printJSON writes alike. Bodies
// that differ may share a print, and are then compared. ok is false where a
// scalar is one redacted reports (the recorded side's), or a number does not
// parse.
func bodyPrint(body []byte, redacted func(string) bool) (uint64, bool) {
	h := fnv.New64a()
	v, isJSON := decodeJSONDoc(body)
	if !isJSON {
		_, _ = h.Write([]byte{'R'})
		_, _ = h.Write(body)
		return h.Sum64(), true
	}
	_, _ = h.Write([]byte{'J'})
	if !printJSON(h, v, redacted) {
		return 0, false
	}
	return h.Sum64(), true
}

// printJSON writes a decoded JSON value to h: an object by its members in key
// order, an array by its elements in order, each with its length, a string by
// its text, and a number by the float64 nearest its value (strconv.ParseFloat
// rounds to nearest), with -0 written as 0, so numbers sameScalar holds equal
// (equal values) are written alike. false where redacted reports a scalar, or
// a number does not parse.
func printJSON(h hash.Hash64, v interface{}, redacted func(string) bool) bool {
	var buf [binary.MaxVarintLen64]byte
	tag := func(t byte, n int) {
		buf[0] = t
		_, _ = h.Write(buf[:1+binary.PutUvarint(buf[1:], uint64(n))])
	}
	switch t := v.(type) {
	case map[string]interface{}:
		tag('o', len(t))
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			tag('k', len(key))
			_, _ = io.WriteString(h, key)
			if !printJSON(h, t[key], redacted) {
				return false
			}
		}
		return true
	case []interface{}:
		tag('a', len(t))
		for _, e := range t {
			if !printJSON(h, e, redacted) {
				return false
			}
		}
		return true
	}
	if redacted != nil && redacted(scalarText(v)) {
		return false
	}
	switch t := v.(type) {
	case string:
		tag('s', len(t))
		_, _ = io.WriteString(h, t)
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return false
		}
		if f == 0 {
			f = 0 // -0 and 0 are one value
		}
		buf[0] = 'n'
		binary.LittleEndian.PutUint64(buf[1:9], math.Float64bits(f))
		_, _ = h.Write(buf[:9])
	case bool:
		if t {
			tag('t', 0)
		} else {
			tag('f', 0)
		}
	case nil:
		tag('z', 0)
	default:
		return false
	}
	return true
}

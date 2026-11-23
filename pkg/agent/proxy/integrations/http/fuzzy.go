package http

import (
	"context"
	"errors"
	"math"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/agnivade/levenshtein"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

const (
	// levenshteinMaxCells bounds the Levenshtein ranking of one request's
	// candidates: the sum, over the candidates, of the two bodies' lengths
	// multiplied once their common start and end are trimmed, which is how
	// many steps the distances take. ~16.7M steps take tens of milliseconds.
	levenshteinMaxCells = 1 << 24

	// heavyCallBytes is the most a call may weigh (admit) and be matched
	// beside any number of others: its body and the recordings of its
	// method, whose bodies the passes below read, some more than once.
	heavyCallBytes = 1 << 20

	// maxBagShingles bounds what a ranking holds of its request (shingleBag).
	maxBagShingles = 1 << 16
)

// clientPollEvery is how often a heavy call asks whether its client left.
var clientPollEvery = 100 * time.Millisecond

// errClientGone is what a match returns when its client left before it ended.
var errClientGone = errors.New("the client left before its call was matched")

// heavyTurns holds a turn for each heavy call being matched: GOMAXPROCS of
// them, read at the first heavy call. Under a CPU limit that is the limit
// rounded up, and at least 2. Go follows a later change of the limit in
// GOMAXPROCS, but the turns keep the count they were made with.
var heavyTurns = sync.OnceValue(func() chan struct{} {
	return make(chan struct{}, runtime.GOMAXPROCS(0))
})

// admit gives a call that weighs over heavyCallBytes its turn: it waits until
// a turn is free (heavyTurns), holding nothing but its request. A client that
// times out and calls again would otherwise add a call beside each one still
// being matched, each with its share of the CPU and its own copies of what it
// reads, until the agent is killed for memory. So its wait and its match both
// end with its client (watchClient). That leaves at most one heavy call in
// hand for each connection still open, and one running for each turn, whatever
// the number of clients.
//
// The context returned is the call's, for the match. end gives the turn back,
// and must be called once the match is over. A lighter call takes no turn and
// is not watched: it runs to its end, past its client if that one left.
func admit(ctx context.Context, client net.Conn, weight int64) (_ context.Context, end func(), err error) {
	if weight <= heavyCallBytes {
		return ctx, func() {}, nil
	}
	ctx, stop := watchClient(ctx, client)
	select {
	case heavyTurns() <- struct{}{}:
		return ctx, func() {
			<-heavyTurns()
			stop()
		}, nil
	case <-ctx.Done():
		stop()
		return ctx, nil, ctx.Err()
	}
}

// watchClient cancels the context it returns once client has ended its side
// of the connection, which a call in hand reads nothing from (pUtil.PeerGone:
// the kernel's state of the socket, which a TLS client's close_notify before
// its FIN does not hide, and takes nothing from it). As in net/http's server,
// a client that only shut its sending side counts as gone. stop ends the
// watch. Where the socket cannot be asked (no client, or a platform with no
// such state) the call is matched to its end.
func watchClient(ctx context.Context, client net.Conn) (_ context.Context, stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	if client == nil {
		return ctx, cancel
	}
	go func() {
		tick := time.NewTicker(clientPollEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if gone, _ := pUtil.PeerGone(client); gone {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

// recordedBytes sums the body lengths of mocks of method, or of every mock
// when method is empty.
func recordedBytes(mocks []*models.Mock, method models.Method) int64 {
	var n int64
	for _, m := range mocks {
		if method == "" || m.Spec.HTTPReq.Method == method {
			n += int64(len(m.Spec.HTTPReq.Body))
		}
	}
	return n
}

// fuzzyBody is the recording's body with its noisy values stripped, so that
// redacted padding doesn't skew a score. A ranking reads it candidate by
// candidate and keeps none: with noise it is a copy of each.
func fuzzyBody(m *models.Mock) string {
	return util.StripNoisyJSON(m.Spec.HTTPReq.Body, util.NewNoiseChecker(m.Noise))
}

// trimCommonEnds cuts what a and b share at their start and then at their
// end, which leaves their Levenshtein distance as it was: a body that drifted
// in one place costs that place, not its length squared.
func trimCommonEnds(a, b string) (string, string) {
	i, n := 0, min(len(a), len(b))
	for i < n && a[i] == b[i] {
		i++
	}
	a, b = a[i:], b[i:]
	j, n := 0, n-i
	for j < n && a[len(a)-1-j] == b[len(b)-1-j] {
		j++
	}
	return a[:len(a)-j], b[:len(b)-j]
}

// levenshteinFits reports whether req, an ASCII body, can be ranked against
// mocks by Levenshtein distance: the steps of what is left of each pair once
// trimmed (trimCommonEnds) fit levenshteinMaxCells, and no such part is longer
// than the 65535 characters the library counts a distance in (it wraps past
// that, and a distance of 65536 reads as an exact match). Non-ASCII recordings
// are not ranked by it (findStringMatch), so they do not count. Unbounded,
// each 200 KB pair took about a minute of CPU. A cancelled ctx does not fit.
func levenshteinFits(ctx context.Context, req string, mocks []*models.Mock) bool {
	var cells int64
	for _, m := range mocks {
		if ctx.Err() != nil {
			return false
		}
		body := fuzzyBody(m)
		if !util.IsASCII(body) {
			continue
		}
		a, b := trimCommonEnds(req, body)
		if len(a) > math.MaxUint16 || len(b) > math.MaxUint16 {
			return false
		}
		if cells += int64(len(a)) * int64(len(b)); cells > levenshteinMaxCells {
			return false
		}
	}
	return true
}

// findStringMatch returns the index of the ASCII recording closest to req
// and its Levenshtein distance, or -1 when no recording is ASCII.
func findStringMatch(ctx context.Context, req string, mocks []*models.Mock) (idx, dist int, err error) {
	idx, dist = -1, math.MaxInt
	for i, m := range mocks {
		if err := ctx.Err(); err != nil {
			return -1, 0, err
		}
		body := fuzzyBody(m)
		if !util.IsASCII(body) {
			continue
		}
		a, b := trimCommonEnds(req, body)
		if d := levenshtein.ComputeDistance(a, b); d < dist {
			idx, dist = i, d
		}
	}
	return idx, dist, nil
}

// shingleBag is what a ranking holds of its request: its shingles, each with
// the times it occurs. Counting them is what tells apart recordings that
// differ only in how many times a row repeats, which a set of shingles
// cannot. A shingle is at most 8 bytes, held as the integer of its bytes. A
// body with more distinct shingles than maxBagShingles keeps an even sample
// of them, by value, so that a recording's are sampled alike: the bag's size
// does not grow with the body's.
type shingleBag struct {
	k int
	// sample keeps a shingle whose mix has none of these bits set.
	sample uint64
	// slots numbers the kept shingles.
	slots map[uint64]int32
	// want is how many times the request holds each, and seen how many
	// times the recording in hand does, up to want.
	want, seen []int32
	// total is want's sum.
	total int
}

// eachShingle calls f with each k-byte shingle of s that sample keeps.
func eachShingle(s string, k int, sample uint64, f func(v uint64) bool) bool {
	mask := ^uint64(0) >> (64 - 8*k)
	var v uint64
	for i := 0; i < len(s); i++ {
		v = (v<<8 | uint64(s[i])) & mask
		if i >= k-1 && (v*0x9E3779B97F4A7C15)>>32&sample == 0 && !f(v) {
			return false
		}
	}
	return true
}

func newShingleBag(req string) *shingleBag {
	b := &shingleBag{k: util.AdaptiveK(len(req), 3, 8, 5)}
	// Each pass that overflows keeps half as many shingles as the last.
	for {
		b.slots, b.want, b.total = make(map[uint64]int32), b.want[:0], 0
		fits := eachShingle(req, b.k, b.sample, func(v uint64) bool {
			i, ok := b.slots[v]
			if !ok {
				if len(b.want) == maxBagShingles {
					return false
				}
				i = int32(len(b.want))
				b.slots[v] = i
				b.want = append(b.want, 0)
			}
			b.want[i]++
			b.total++
			return true
		})
		if fits {
			b.seen = make([]int32, len(b.want))
			return b
		}
		b.sample = b.sample<<1 | 1
	}
}

// similarity is the Jaccard similarity of the request's and body's shingles,
// counted as multisets: those they share, repeats included, over all of them.
func (b *shingleBag) similarity(body string) float64 {
	clear(b.seen)
	common, total := 0, 0
	eachShingle(body, b.k, b.sample, func(v uint64) bool {
		total++
		if i, ok := b.slots[v]; ok && b.seen[i] < b.want[i] {
			b.seen[i]++
			common++
		}
		return true
	})
	union := b.total + total - common
	if union == 0 {
		return 0
	}
	return float64(common) / float64(union)
}

// PerformFuzzyMatch picks the recording closest to reqBuff, in its turn when
// the call is heavy (admit), for a caller with no client to watch.
func (h *HTTP) PerformFuzzyMatch(tcsMocks []*models.Mock, reqBuff []byte) (bool, *models.Mock) {
	// A context that never ends takes a turn with no error.
	ctx, end, _ := admit(context.Background(), nil, int64(len(reqBuff))+recordedBytes(tcsMocks, ""))
	defer end()
	ok, m, _ := h.performFuzzyMatch(ctx, tcsMocks, reqBuff)
	return ok, m
}

// performFuzzyMatch picks the recording closest to body: by Levenshtein
// distance while its steps fit their bound (levenshteinFits), else by the
// similarity of their shingles (shingleBag), in time linear in their lengths
// and holding one recording at a time. It stops with ctx's error when ctx
// ends. Noisy (obfuscated) values are stripped from the recordings' bodies
// (fuzzyBody).
func (h *HTTP) performFuzzyMatch(ctx context.Context, tcsMocks []*models.Mock, body []byte) (bool, *models.Mock, error) {
	// Log all mock names in a single line for better readability
	if ce := h.Logger.Check(zap.DebugLevel, "mocks under consideration for performfuzzyMatch function"); ce != nil {
		ce.Write(zap.Strings("mock names", mockNamesOf(tcsMocks)))
	}

	req := string(body)
	for _, mock := range tcsMocks {
		if mock.Spec.HTTPReq.Body == req {
			if ce := h.Logger.Check(zap.DebugLevel, "http mock matched"); ce != nil {
				ce.Write(zap.String("mock", mock.Name),
					zap.Float64("match_percentage", 100.0),
					zap.String("match_type", "fuzzy_exact"))
			}
			return true, mock, nil
		}
	}

	// String-based fuzzy matching (Levenshtein distance)
	if util.IsASCII(req) && levenshteinFits(ctx, req, tcsMocks) {
		idx, dist, err := findStringMatch(ctx, req, tcsMocks)
		if err != nil {
			return false, nil, err
		}
		if idx != -1 {
			if ce := h.Logger.Check(zap.DebugLevel, "http mock matched"); ce != nil {
				maxLen := max(len(req), len(fuzzyBody(tcsMocks[idx])))
				pct := 0.0
				if maxLen > 0 {
					pct = (1.0 - float64(dist)/float64(maxLen)) * 100
				}
				ce.Write(zap.String("mock", tcsMocks[idx].Name),
					zap.Float64("match_percentage", pct),
					zap.String("match_type", "fuzzy_levenshtein"))
			}
			return true, tcsMocks[idx], nil
		}
	}

	// Binary fuzzy matching (shingle similarity)
	bag := newShingleBag(req)
	mxIdx, mxSim := -1, -1.0
	for i, m := range tcsMocks {
		if err := ctx.Err(); err != nil {
			return false, nil, err
		}
		if sim := bag.similarity(fuzzyBody(m)); sim > mxSim {
			mxIdx, mxSim = i, sim
		}
	}
	if mxIdx != -1 {
		if ce := h.Logger.Check(zap.DebugLevel, "http mock matched"); ce != nil {
			ce.Write(zap.String("mock", tcsMocks[mxIdx].Name),
				zap.Float64("match_percentage", mxSim*100),
				zap.String("match_type", "fuzzy_jaccard"))
		}
		return true, tcsMocks[mxIdx], nil
	}
	return false, nil, nil
}

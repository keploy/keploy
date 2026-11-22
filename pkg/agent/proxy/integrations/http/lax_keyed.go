package http

import (
	"errors"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// pathIndex files each HTTP recording under its method and recorded path
// (pathKey), which a call's first schema pass with no url noise compares, and
// under its method and its path with every id-shaped segment masked (idKey),
// which the second pass, of auto-detected id segments, compares. With the
// proven switch off a call reads, for each pass, the recordings filed under
// its own key (keyedLaxRead), not the whole pool.
var pathIndex = &integrations.MockIndex{Keys: requestPathKeys}

func requestPathKeys(m *models.Mock) []string {
	method, path, ok := indexedRequest(m)
	if !ok {
		return nil
	}
	return []string{pathKey(method, path), idKey(method, path)}
}

// indexedRequest is the method and recorded path of m, an HTTP recording;
// false for any other mock and for a URL that does not parse, which no call's
// path matches (MatchURLPath).
func indexedRequest(m *models.Mock) (string, string, bool) {
	if m == nil || m.Kind != models.Kind(models.HTTP) || m.Spec.HTTPReq == nil {
		return "", "", false
	}
	path, ok := recordedURLPath(m.Spec.HTTPReq.URL)
	return string(m.Spec.HTTPReq.Method), path, ok
}

// pathKey is the key of a request by its method and path. A method holds no
// space.
func pathKey(method, path string) string {
	return "p" + method + " " + path
}

// idKey is the key of a request by its method and its path with each segment
// that looks like an id (urlSegmentTypeClass) written as its class. Two paths
// the second pass relates (pathMatchesModuloDynamicSegments: as many
// segments, each equal or of one id class) have one key, and so does a path
// with itself; other paths can share one too (a segment that reads as a
// class's mark), and the schema match tells them apart.
func idKey(method, path string) string {
	var b strings.Builder
	b.Grow(len(method) + len(path) + 2)
	b.WriteString("i")
	b.WriteString(method)
	b.WriteByte(' ')
	for i, seg := range strings.Split(path, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		if c := urlSegmentTypeClass(seg); c != "" {
			b.WriteByte(0)
			b.WriteString(c)
			continue
		}
		b.WriteString(seg)
	}
	return b.String()
}

// noiseIndexes holds an index for each url noise a call was matched under
// (noiseIndexOf), by its patterns in order, at most noiseIndexesCap of them.
var noiseIndexes struct {
	mu sync.Mutex
	m  map[string]*integrations.MockIndex
}

const noiseIndexesCap = 16

// noiseIndexOf is the index that files each HTTP recording under its method
// and its recorded path with urlNoise's matches masked (maskURLNoise), which
// the first schema pass of a call under urlNoise compares: a path matches
// under it when the two masked paths are one, and a path is one with itself.
func noiseIndexOf(logger *zap.Logger, urlNoise []string) *integrations.MockIndex {
	cfg := strings.Join(urlNoise, "\x00")
	noiseIndexes.mu.Lock()
	defer noiseIndexes.mu.Unlock()
	if ix := noiseIndexes.m[cfg]; ix != nil {
		return ix
	}
	if noiseIndexes.m == nil || len(noiseIndexes.m) >= noiseIndexesCap {
		noiseIndexes.m = map[string]*integrations.MockIndex{}
	}
	res := compileURLNoise(logger, urlNoise)
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string {
		method, path, ok := indexedRequest(m)
		if !ok {
			return nil
		}
		return []string{pathKey(method, maskURLNoise(path, res))}
	}}
	noiseIndexes.m[cfg] = ix
	return ix
}

// errKeyedRead is a keyed read the store failed; the whole read reports it.
var errKeyedRead = errors.New("error while matching the request with the mocks")

// keyedLaxRead is what laxPasses read for a call with the proven switch off
// through the stores' key indexes, in place of the whole pool: the per-test,
// then the session recordings the index files under the call's key, in the
// pool's order, less what is spent. The schema match keeps or drops each
// recording on its own, so over recordings that hold every one of the pool
// that a pass's path check passes, it keeps what it keeps of the pool, in the
// same order, and every later pass is handed what it is handed now:
//   - the first pass, with no url noise, compares the recordings of the
//     call's method and path (pathKey); under url noise, those of its method
//     and noise-masked path (noiseIndexOf);
//   - the second, those of the call's method and id-masked path (idKey),
//     which hold every recording whose path is the call's or one an id
//     segment relates. Under url noise it also compares the first pass's
//     recordings, so their id-masked path must be the call's too: else the
//     second pass reads the whole pool;
//   - a miss is reported against the whole pool, read then, as the whole read
//     reports it: its count, its destinations and its closest recording.
//
// ok is false, and the call reads the whole pool, under exact serving, which
// reads by the test's mapping, or when the store reads no key.
func (h *HTTP) keyedLaxRead(input *req, mockDb integrations.MockMemDb, urlNoise []string) (laxRead, func(*models.Mock) bool, bool) {
	if provenswitch.On() || integrations.ExactServing(mockDb) {
		return laxRead{}, nil, false
	}
	if _, exact := integrations.SequenceViewOf(mockDb); exact {
		return laxRead{}, nil, false
	}
	pr, ok := mockDb.(integrations.PerTestKeyRanger)
	if !ok {
		return laxRead{}, nil, false
	}
	sr, ok := mockDb.(integrations.SessionKeyReader)
	if !ok {
		return laxRead{}, nil, false
	}
	read := func(ix *integrations.MockIndex, key string) ([]*models.Mock, []*models.Mock, error) {
		var keyed []*models.Mock
		add := func(mk *models.Mock) bool {
			keyed = append(keyed, mk)
			return true
		}
		if err := pr.RangePerTestMocksWithKey(ix, key, add); err != nil {
			utils.LogError(h.Logger, err, "failed to get per-test mocks")
			return nil, nil, errKeyedRead
		}
		perTest := keyed[:len(keyed):len(keyed)]
		if err := sr.RangeSessionMocksWithKey(ix, key, add); err != nil {
			utils.LogError(h.Logger, err, "failed to get session mocks")
			return nil, nil, errKeyedRead
		}
		return FilterHTTPMocks(integrations.WithoutSpent(mockDb, keyed)), perTest, nil
	}
	method, path := input.method, input.url.Path
	ix, key := pathIndex, pathKey(method, path)
	if len(urlNoise) > 0 {
		ix, key = noiseIndexOf(h.Logger, urlNoise), pathKey(method, maskURLNoise(path, compileURLNoise(h.Logger, urlNoise)))
	}
	pass1, perTest, err := read(ix, key)
	if err != nil {
		return laxRead{}, nil, false
	}
	whole := func() ([]*models.Mock, error) {
		perTestMocks, err := mockDb.GetPerTestMocksInWindow()
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get per-test mocks")
			return nil, errKeyedRead
		}
		sessionMocks, err := mockDb.GetSessionMocks()
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get session mocks")
			return nil, errKeyedRead
		}
		combined := make([]*models.Mock, 0, len(perTestMocks)+len(sessionMocks))
		combined = append(combined, perTestMocks...)
		combined = append(combined, sessionMocks...)
		return FilterHTTPMocks(integrations.WithoutSpent(mockDb, combined)), nil
	}
	rd := laxRead{
		pass1: pass1,
		pass2: func() ([]*models.Mock, error) {
			id := idKey(method, path)
			for _, m := range pass1 {
				if mm, mp, _ := indexedRequest(m); idKey(mm, mp) != id {
					return whole()
				}
			}
			pass2, _, err := read(pathIndex, id)
			return pass2, err
		},
		missed: func(d *matchDiag) (*matchDiag, error) {
			pool, err := whole()
			if err != nil {
				return nil, err
			}
			if len(pool) == 0 {
				return &matchDiag{phase: models.MatchPhaseNoMocks}, nil
			}
			d.candidates, d.pool = len(pool), pool
			return d, nil
		},
	}
	return rd, openTestOwner(mockDb, perTest), true
}

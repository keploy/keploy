package learn

import (
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestMain runs the package's rows in the modes keploy's CI runs them in
// besides the plain one:
//   - KEPLOY_LEARN_LAZY_EVERY_RAW=1 (everyRawLazy): each RawOf a lazy Raw of
//     the same identity whose payload is read again at each use, from a store
//     that keeps none;
//   - KEPLOY_LEARN_SET_EVERY_CACHE=1 (everyCacheSet): each NewDriftCache with
//     a set cache of its own, as a tracker's test has, so that what the set
//     keeps of a recording is read where most rows read a cache no tracker
//     made.
func TestMain(m *testing.M) {
	if os.Getenv("KEPLOY_LEARN_LAZY_EVERY_RAW") == "1" {
		store := newBodyStore(0)
		var n atomic.Int64
		everyRawLazy = func(mock *models.Mock, served bool) Raw {
			r := rawOfNow(mock, served)
			key := "every\x00" + strconv.FormatInt(n.Add(1), 10)
			r.rText, r.rClock, r.rFields, r.rStamps, r.rRead = "", nil, nil, nil, false
			r.src = &rawSource{id: key, name: mock.Name, load: func() (*models.Mock, error) { return mock, nil }, store: store}
			return r
		}
	}
	everyCacheSet = os.Getenv("KEPLOY_LEARN_SET_EVERY_CACHE") == "1"
	os.Exit(m.Run())
}

// noSet is dc with no set cache, in every mode (everyCacheSet): a cache no
// tracker made, what a row compares a tracker's test with.
func noSet(dc *DriftCache) *DriftCache {
	dc.set = nil
	return dc
}

package learn

import (
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestMain runs the package's rows over lazy Raws where
// KEPLOY_LEARN_LAZY_EVERY_RAW=1 (everyRawLazy): each RawOf a lazy Raw of
// the same identity whose payload is read again at each use, from a store
// that keeps none.
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
	os.Exit(m.Run())
}

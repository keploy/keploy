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

// Each mode CI runs the package's rows in is on where its variable names it,
// and off where none does. A mode its variable no longer reaches (a
// NewDriftCache with no set cache of its own, a RawOf read when made) turns
// its CI step into a second plain run, and the mutants the step is there to
// kill (a part of a recording the set keeps under another part's key, a
// payload field read past the accessor) survive CI again.
func TestEachCorpusModeIsOnWhereItsVariableNamesIt(t *testing.T) {
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://example.com/a"},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "a"},
	}}
	if want, got := os.Getenv("KEPLOY_LEARN_SET_EVERY_CACHE") == "1", NewDriftCache().set != nil; got != want {
		t.Errorf("KEPLOY_LEARN_SET_EVERY_CACHE=%q: NewDriftCache gives a set cache of its own: %v, want %v", os.Getenv("KEPLOY_LEARN_SET_EVERY_CACHE"), got, want)
	}
	if want, got := os.Getenv("KEPLOY_LEARN_LAZY_EVERY_RAW") == "1", RawOf(m, true).src != nil; got != want {
		t.Errorf("KEPLOY_LEARN_LAZY_EVERY_RAW=%q: RawOf makes a lazy Raw: %v, want %v", os.Getenv("KEPLOY_LEARN_LAZY_EVERY_RAW"), got, want)
	}
	if !RawOf(m, true).isRead() {
		t.Errorf("RawOf of a mock it can read reads as unread: the row pins nothing")
	}
}

package http

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func laneNotify() models.AsyncLane {
	return models.AsyncLane{
		Name:           "notifications",
		Type:           "http",
		Match:          map[string]string{"host": "notify.internal.svc", "path": "/v1/poll*"},
		VolatileParams: []string{"cursor"},
	}
}

func TestMatchesLaneHostPathGlob(t *testing.T) {
	h := newHTTP()
	m := httpMock("m1", "GET", "http://notify.internal.svc/v1/poll?cursor=5")
	if !h.MatchesLane(m, laneNotify()) {
		t.Fatal("expected lane match on host+path glob")
	}
	other := httpMock("m2", "GET", "http://api.other.svc/v2/users")
	if h.MatchesLane(other, laneNotify()) {
		t.Fatal("non-lane host must not match")
	}
}

func TestEmptyResponseIs204(t *testing.T) {
	h := newHTTP()
	b, err := h.EmptyResponse(laneNotify())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got[:12] != "HTTP/1.1 204" {
		t.Fatalf("keep-alive should be 204, got %q", got[:20])
	}
}

func TestMatchRequestShapeVolatileParamIgnored(t *testing.T) {
	h := newHTTP()
	recorded := httpMock("rec", "GET", "http://notify.internal.svc/v1/poll?cursor=1")
	live := httpMock("live", "GET", "http://notify.internal.svc/v1/poll?cursor=999")
	ok, detail := h.MatchRequestShape(live, recorded, laneNotify())
	if !ok {
		t.Fatalf("volatile cursor difference must not fail shape: %s", detail)
	}
}

func TestMatchRequestShapePathDriftFlags(t *testing.T) {
	h := newHTTP()
	recorded := httpMock("rec", "GET", "http://notify.internal.svc/v1/poll?cursor=1")
	live := httpMock("live", "GET", "http://notify.internal.svc/v1/DIFFERENT?cursor=1")
	ok, _ := h.MatchRequestShape(live, recorded, laneNotify())
	if ok {
		t.Fatal("path drift must report shape mismatch")
	}
}

// TestMatchRequestShapeStripsRecordedURLParams exercises the recorded-side
// branch of stripVolatile (`if req.URLParams != nil { ... }`), which the
// other MatchRequestShape tests never touch because httpMock() leaves
// HTTPReq.URLParams nil. At record time pkg.URLParams(req) populates
// URLParams for real, so SchemaMatch's query-param check
// (MapsHaveSameKeys(mock.Spec.HTTPReq.URLParams, input.url.Query())) compares
// against a real recorded URLParams map in production. Here the recorded
// mock's URLParams includes the volatile "cursor" key alongside a stable
// "page" key; the live request differs only in cursor's value. If the
// recorded-side strip did not run, recorded.Spec.HTTPReq.URLParams would
// still carry "cursor" (key count 2) while the live side's query (parsed
// from its already-stripped URL) would only carry "page" (key count 1),
// and MapsHaveSameKeys would report a spurious key-count mismatch.
func TestMatchRequestShapeStripsRecordedURLParams(t *testing.T) {
	h := newHTTP()
	recorded := &models.Mock{
		Name: "rec",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: models.Method("GET"),
				URL:    "http://notify.internal.svc/v1/poll?cursor=1&page=2",
				URLParams: map[string]string{
					"cursor": "1",
					"page":   "2",
				},
			},
		},
	}
	origURLParams := map[string]string{"cursor": "1", "page": "2"}

	// Live mock differs only in the volatile cursor's value/presence.
	live := httpMock("live", "GET", "http://notify.internal.svc/v1/poll?cursor=999&page=2")

	ok, detail := h.MatchRequestShape(live, recorded, laneNotify())
	if !ok {
		t.Fatalf("expected recorded-side URLParams volatile strip to allow shape match: %s", detail)
	}

	// stripVolatile must shallow-copy, never mutate the original recorded mock.
	if !reflect.DeepEqual(recorded.Spec.HTTPReq.URLParams, origURLParams) {
		t.Errorf("stripVolatile mutated the original recorded mock's URLParams: got %v, want %v",
			recorded.Spec.HTTPReq.URLParams, origURLParams)
	}
}

// TestMatchRequestShapeRecordedURLParamsKeyDrift is the negative companion:
// the recorded mock's URLParams and the live request's query differ on a
// NON-volatile key ("page" vs "other"), so the strip must not paper over a
// genuine shape drift — proving stripVolatile only removes the volatile key
// and doesn't over-strip the recorded-side URLParams comparison.
func TestMatchRequestShapeRecordedURLParamsKeyDrift(t *testing.T) {
	h := newHTTP()
	recorded := &models.Mock{
		Name: "rec",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: models.Method("GET"),
				URL:    "http://notify.internal.svc/v1/poll?cursor=1&page=2",
				URLParams: map[string]string{
					"cursor": "1",
					"page":   "2",
				},
			},
		},
	}

	// "page" replaced by "other" — a non-volatile key drift.
	live := httpMock("live", "GET", "http://notify.internal.svc/v1/poll?cursor=999&other=3")

	ok, _ := h.MatchRequestShape(live, recorded, laneNotify())
	if ok {
		t.Fatal("non-volatile query key drift (page vs other) must report shape mismatch")
	}
}

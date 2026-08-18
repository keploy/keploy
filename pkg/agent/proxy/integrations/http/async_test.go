package http

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
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

func TestBuildMockResponseBytes(t *testing.T) {
	h := newHTTP()
	// A stale Content-Length (999) in the recorded header must be recomputed to
	// the actual body length. Note: the serializer only *overrides* an existing
	// Content-Length key; it does not synthesize one when absent — this mirrors
	// the original inline replay path exactly (byte-identity constraint).
	stub := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{ProtoMajor: 1, ProtoMinor: 1},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "hello", Header: map[string]string{"Content-Type": "text/plain", "Content-Length": "999"}},
	}}
	out, err := h.buildMockResponseBytes(stub)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("bad status line: %q", s)
	}
	if !strings.Contains(s, "Content-Length: 5\r\n") {
		t.Fatalf("content-length not recomputed: %q", s)
	}
	if !strings.HasSuffix(s, "\r\n\r\nhello") {
		t.Fatalf("body not appended: %q", s)
	}
}

func TestLiveReqToMockCarriesMethodURLBody(t *testing.T) {
	u, _ := url.Parse("http://notify.internal.svc/v1/poll?cursor=7")
	hdr := http.Header{}
	hdr.Set("X-A", "b")
	in := &req{method: "GET", url: u, header: hdr, body: []byte("q")}
	m := liveReqToMock(in)
	if m.Spec.HTTPReq.Method != "GET" || m.Spec.HTTPReq.URL != u.String() ||
		m.Spec.HTTPReq.Body != "q" || m.Spec.HTTPReq.Header["X-A"] != "b" {
		t.Fatalf("liveReqToMock lost fields: %+v", m.Spec.HTTPReq)
	}
}

func TestMatchesLaneQueryFlag(t *testing.T) {
	h := newHTTP()
	lane := models.AsyncLane{
		Name: "config-watch", Type: "http",
		Match:      map[string]string{"path": "/v1/buckets/stream-relay"},
		MatchQuery: map[string]string{"watch": "true"},
	}
	poll := httpMock("p", "GET", "http://cfg/v1/buckets/stream-relay?watch=true&version=3")
	boot := httpMock("b", "GET", "http://cfg/v1/buckets/stream-relay?watch=false")
	if !h.MatchesLane(poll, lane) {
		t.Fatal("watch=true long-poll should match the lane")
	}
	if h.MatchesLane(boot, lane) {
		t.Fatal("watch=false boot 'get current' call must NOT match the lane")
	}
}

func TestMatchesLanePathRegex(t *testing.T) {
	h := newHTTP()
	lane := models.AsyncLane{
		Name: "tenant-rules", Type: "http",
		Match: map[string]string{"pathRegex": "^/v1/tenant/[0-9]+/rules/[A-Z_]+$"},
	}
	good := httpMock("g", "GET", "http://svc/v1/tenant/42/rules/LAST_MILE")
	bad := httpMock("b", "GET", "http://svc/v1/tenant/x/rules/lower")
	if !h.MatchesLane(good, lane) {
		t.Fatal("numeric tenant + upper-case use-case should match the regex lane")
	}
	if h.MatchesLane(bad, lane) {
		t.Fatal("non-numeric tenant / lower-case use-case must not match the regex lane")
	}
}

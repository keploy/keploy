package http

import (
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

package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type listenAddrsSvc struct {
	agent.Service // nil: any call other than AppListenAddrs panics loudly
	addrs         map[uint16][]netip.Addr
	err           error
}

func (s listenAddrsSvc) AppListenAddrs(_ context.Context, port uint16) ([]netip.Addr, error) {
	return s.addrs[port], s.err
}

// The CLI reads anything but a 200 as "the agent cannot tell", which leaves its
// advice as it was; a 200 is a statement about where the app listens, so only
// an answer the agent has may be one.
func TestHandleAppListenAddrs(t *testing.T) {
	get := func(svc agent.Service, query string) (int, models.AppListenAddrs) {
		r := chi.NewRouter()
		DefaultRoutes{}.New(r, svc, zap.NewNop())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/app/listen-addrs"+query, nil))
		var body models.AppListenAddrs
		if rec.Code == http.StatusOK {
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return rec.Code, body
	}

	svc := listenAddrsSvc{addrs: map[uint16][]netip.Addr{
		8097: {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")},
	}}
	if code, body := get(svc, "?port=8097"); code != http.StatusOK || !slices.Equal(body.Addrs, []string{"127.0.0.1", "::1"}) {
		t.Errorf("got %d %v, want 200 [127.0.0.1 ::1]", code, body.Addrs)
	}
	if code, body := get(svc, "?port=8096"); code != http.StatusOK || body.Addrs == nil || len(body.Addrs) != 0 {
		t.Errorf("nothing listening: got %d %#v, want 200 and an empty list", code, body.Addrs)
	}
	for _, q := range []string{"", "?port=0", "?port=70000", "?port=x"} {
		if code, _ := get(svc, q); code != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400", q, code)
		}
	}
	if code, _ := get(listenAddrsSvc{err: errors.New("cannot tell")}, "?port=8097"); code != http.StatusInternalServerError {
		t.Errorf("a service that cannot tell: got %d, want 500", code)
	}
	if code, _ := get(nil, "?port=8097"); code != http.StatusNotImplemented {
		t.Errorf("a service without the question: got %d, want 501", code)
	}
}

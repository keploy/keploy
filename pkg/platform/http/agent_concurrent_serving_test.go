package http_test

// Under the proven switch an agent that serves a test the recordings it shares
// with the tests that ran beside it at record time says so on its /storemocks
// answer (models.ExactConcurrentHeader), and the client sends them only to an
// agent that said so. These run the real routes against the real client.

import (
	"context"
	"encoding/gob"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// concSvc is an agent service whose proxy serves shared recordings, or not.
type concSvc struct {
	stubSvc
	serves bool
}

func (s *concSvc) ServesExactConcurrent() bool { return s.serves }

func serveConc(t *testing.T, svc *concSvc) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestTheConcurrentServingCapabilityComesFromStoreMocks(t *testing.T) {
	ctx := context.Background()
	f, u := fixtures()
	t.Run("an agent that serves them says so under the switch", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		c := newClient(t, serveConc(t, &concSvc{serves: true}).URL+"/agent")
		if c.ServesConcurrent() {
			t.Fatal("believed before any store")
		}
		if err := c.StoreMocks(ctx, f, u); err != nil {
			t.Fatal(err)
		}
		if !c.ServesConcurrent() {
			t.Fatal("the agent said it serves shared recordings, and the client did not take it")
		}
	})
	t.Run("an agent whose proxy does not serve them says nothing", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		c := newClient(t, serveConc(t, &concSvc{serves: false}).URL+"/agent")
		if err := c.StoreMocks(ctx, f, u); err != nil {
			t.Fatal(err)
		}
		if c.ServesConcurrent() {
			t.Fatal("an agent that cannot serve shared recordings was believed to")
		}
	})
	t.Run("with the switch off the wire is unchanged", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		srv := serveConc(t, &concSvc{serves: true})
		c := newClient(t, srv.URL+"/agent")
		if err := c.StoreMocks(ctx, f, u); err != nil {
			t.Fatal(err)
		}
		if c.ServesConcurrent() {
			t.Fatal("believed with the switch off")
		}
	})
	t.Run("each store starts afresh, the legacy framing included", func(t *testing.T) {
		send, fail := true, false
		mux := http.NewServeMux()
		mux.HandleFunc("/agent/storemocks", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Content-Type") == models.StoreMocksStreamContentType {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var req models.StoreMocksReq
			_ = gob.NewDecoder(r.Body).Decode(&req)
			if fail {
				// An agent started again in its place, which holds nothing
				// yet: the store fails, and what the agent before said is
				// no longer known.
				w.WriteHeader(http.StatusInternalServerError)
				_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: false, ErrorMsg: "no session"})
				return
			}
			if send {
				w.Header().Set(models.ExactConcurrentHeader, models.ExactConcurrentServes)
			}
			w.WriteHeader(http.StatusOK)
			_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		c := newClient(t, srv.URL+"/agent")
		if err := c.StoreMocks(ctx, f, u); err != nil {
			t.Fatal(err)
		}
		if !c.ServesConcurrent() {
			t.Fatal("the legacy answer's header was not read")
		}
		send = false
		if err := c.StoreMocks(ctx, f, u); err != nil {
			t.Fatal(err)
		}
		if c.ServesConcurrent() {
			t.Fatal("the capability survived a store whose answer did not say it")
		}
		send = true
		if err := c.StoreMocks(ctx, f, u); err != nil || !c.ServesConcurrent() {
			t.Fatalf("store again: %v, %v", err, c.ServesConcurrent())
		}
		fail = true
		if err := c.StoreMocks(ctx, f, u); err == nil {
			t.Fatal("a failed store was not an error")
		}
		if c.ServesConcurrent() {
			t.Fatal("the capability survived a store that failed")
		}
	})
}

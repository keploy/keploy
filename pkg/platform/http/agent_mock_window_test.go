package http_test

// The agent says, answering each /updatemockparams call, how it served the
// call's window (models.MockWindowHeader): its strict mock window mode, with
// its own KEPLOY_STRICT_MOCK_WINDOW folded in, and its per-test load mode. The
// client folds what the answers since the set's mocks were stored said
// (AgentMockWindow), which the replay's mock prune holds by. These run the real
// routes against the real client.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// windowSvc says how it serves each call's window as the agent service does
// (agentsvc.Agent.MockWindowOf).
type windowSvc struct{ paramsSvc }

func (s *windowSvc) MockWindowOf(p models.MockFilterParams) models.MockWindowMode {
	return (&agentsvc.Agent{}).MockWindowOf(p)
}

func serveWindows(t *testing.T, svc agentsvc.Service) string {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL + "/agent"
}

func TestTheClientHoldsByHowTheAgentSaidItServedTheWindows(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	staging := func(strict, mapping bool) models.MockFilterParams {
		return models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: start.Add(time.Hour), StrictMockWindow: strict, UseMappingBased: mapping}
	}
	window := func(strict, mapping bool) models.MockFilterParams {
		p := models.MockFilterParams{AfterTime: start, BeforeTime: start.Add(2 * time.Second), StrictMockWindow: strict, UseMappingBased: mapping}
		if mapping {
			p.MockMapping = []string{"mock-1"}
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		calls []models.MockFilterParams
		want  models.MockWindowsSaid
	}{
		{"strict, by window", []models.MockFilterParams{staging(true, false), window(true, false)}, models.MockWindowsSaid{Strict: true, Lookahead: true}},
		{"lax, by window", []models.MockFilterParams{staging(false, false), window(false, false)}, models.MockWindowsSaid{}},
		{"lax, by mapping", []models.MockFilterParams{staging(false, true), window(false, true)}, models.MockWindowsSaid{Lookahead: true}},
		// A later answer that says less takes back nothing an earlier one
		// said: a test served by its mapping, then one lax with none.
		{"answers that differ", []models.MockFilterParams{staging(true, false), window(false, true), window(false, false)}, models.MockWindowsSaid{Strict: true, Lookahead: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, serveWindows(t, &windowSvc{}))
			if _, ok := c.AgentMockWindow(); ok {
				t.Fatal("the client holds what an agent it has not asked said")
			}
			for _, p := range tc.calls {
				if err := c.UpdateMockParams(ctx, p); err != nil {
					t.Fatalf("UpdateMockParams: %v", err)
				}
			}
			got, ok := c.AgentMockWindow()
			if !ok || got != tc.want {
				t.Fatalf("AgentMockWindow = %+v, %v; want %+v, true", got, ok, tc.want)
			}
			// A store addresses the next set, possibly a fresh agent: what
			// the answers said before is not its.
			if err := c.StoreMocks(ctx, nil, nil); err != nil {
				t.Fatalf("StoreMocks: %v", err)
			}
			if _, ok := c.AgentMockWindow(); ok {
				t.Fatal("what the agent said for the last set outlives the store of the next")
			}
		})
	}

	t.Run("an agent that does not say", func(t *testing.T) {
		c := newClient(t, serveWindows(t, &paramsSvc{}))
		if err := c.UpdateMockParams(ctx, window(true, false)); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		if got, ok := c.AgentMockWindow(); ok {
			t.Fatalf("an agent that said nothing was taken to have said %+v", got)
		}
	})

	// An agent in the set's place that stops saying, as one older than the
	// header started again mid-set: what the others said is not all of how
	// the set's windows were served.
	t.Run("an agent that stops saying", func(t *testing.T) {
		saying, silent := serveWindows(t, &windowSvc{}), serveWindows(t, &paramsSvc{})
		var calls atomic.Int32
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			to := saying
			if r.URL.Path == "/agent/updatemockparams" && calls.Add(1) > 1 {
				to = silent
			}
			u, err := url.Parse(to)
			if err != nil {
				t.Errorf("parse %q: %v", to, err)
				return
			}
			httputil.NewSingleHostReverseProxy(&url.URL{Scheme: u.Scheme, Host: u.Host}).ServeHTTP(w, r)
		}))
		t.Cleanup(front.Close)
		c := newClient(t, front.URL+"/agent")
		for _, p := range []models.MockFilterParams{staging(false, false), window(false, false)} {
			if err := c.UpdateMockParams(ctx, p); err != nil {
				t.Fatalf("UpdateMockParams: %v", err)
			}
		}
		if calls.Load() != 2 {
			t.Fatalf("the agent was asked %d times, want 2", calls.Load())
		}
		if got, ok := c.AgentMockWindow(); ok {
			t.Fatalf("a set whose last answer said nothing was taken to have said %+v", got)
		}
	})
}

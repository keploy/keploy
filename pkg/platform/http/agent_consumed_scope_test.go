package http_test

// The client may send the agent only the per-test entries of the consumed-mock
// history, but only to an agent that has said it reads no others, in the
// X-Keploy-Consumed-Scope header of its /updatemockparams answer. Agents from
// v3.0.0-beta1 through v3.3.22 also applied the history to the session pool, and
// none of them sends the header. These run the real routes against the real
// client.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// paramsSvc is an agent service that takes filter params and says nothing about
// how it reads the consumed history: every agent released before the header.
type paramsSvc struct {
	stubSvc
	fail atomic.Bool
}

func (s *paramsSvc) UpdateMockParams(context.Context, models.MockFilterParams) error {
	if s.fail.Load() {
		return errors.New("no mock session")
	}
	return nil
}

// perTestSvc says it reads the consumed history only for its per-test mocks,
// the way the agent service answers for itself.
type perTestSvc struct{ paramsSvc }

func (s *perTestSvc) ReadsConsumedForPerTestOnly(svc agentsvc.Service) bool {
	return svc == agentsvc.Service(s)
}

func serve(t *testing.T, svc interface {
	UpdateMockParams(context.Context, models.MockFilterParams) error
}) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	switch s := svc.(type) {
	case *perTestSvc:
		routes.DefaultRoutes{}.New(r, s, zap.NewNop())
	case *paramsSvc:
		routes.DefaultRoutes{}.New(r, s, zap.NewNop())
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestConsumedScopeOnlyFromAnAgentThatSaysSo(t *testing.T) {
	ctx := context.Background()
	params := models.MockFilterParams{TotalConsumedMocks: map[string]models.MockState{"m": {Name: "m"}}}

	t.Run("an agent without the header gets the whole history", func(t *testing.T) {
		c := newClient(t, serve(t, &paramsSvc{}).URL+"/agent")
		for i := 0; i < 3; i++ {
			if err := c.UpdateMockParams(ctx, params); err != nil {
				t.Fatalf("UpdateMockParams: %v", err)
			}
			if c.AgentReadsConsumedPerTestOnly() {
				t.Fatal("an agent that never said it reads only per-test entries was treated as if it had")
			}
		}
	})

	t.Run("an agent with the header is believed from its answer on, until the next store", func(t *testing.T) {
		c := newClient(t, serve(t, &perTestSvc{}).URL+"/agent")
		if c.AgentReadsConsumedPerTestOnly() {
			t.Fatal("narrowed before the agent had answered")
		}
		if err := c.UpdateMockParams(ctx, params); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		if !c.AgentReadsConsumedPerTestOnly() {
			t.Fatal("the agent's answer said per-test, and the client did not take it")
		}
		// A store addresses an agent this client may not have heard from: a
		// replacement of another version starts again from the whole history.
		f, u := fixtures()
		if err := c.StoreMocks(ctx, f, u); err != nil {
			t.Fatalf("StoreMocks: %v", err)
		}
		if c.AgentReadsConsumedPerTestOnly() {
			t.Fatal("the scope survived a store; a replaced agent would get a narrowed history before it said anything")
		}
		if err := c.UpdateMockParams(ctx, params); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		if !c.AgentReadsConsumedPerTestOnly() {
			t.Fatal("the scope was not taken again from the next answer")
		}
	})

	t.Run("a call that gets no answer says nothing", func(t *testing.T) {
		srv := serve(t, &perTestSvc{})
		c := newClient(t, srv.URL+"/agent")
		if err := c.UpdateMockParams(ctx, params); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		srv.Close() // the agent goes away
		if err := c.UpdateMockParams(ctx, params); err == nil {
			t.Fatal("precondition: the call was supposed to fail at the transport")
		}
		if c.AgentReadsConsumedPerTestOnly() {
			t.Fatal("the per-test scope survived a call that reached no agent")
		}
	})

	t.Run("a failed answer says nothing", func(t *testing.T) {
		svc := &perTestSvc{}
		c := newClient(t, serve(t, svc).URL+"/agent")
		if err := c.UpdateMockParams(ctx, params); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		svc.fail.Store(true)
		if err := c.UpdateMockParams(ctx, params); err == nil {
			t.Fatal("precondition: the agent was made to fail")
		}
		if c.AgentReadsConsumedPerTestOnly() {
			t.Fatal("a failed answer left the per-test scope in place")
		}
	})

	t.Run("the header is all that changes in the answer", func(t *testing.T) {
		// A client that predates the header reads only the body; it must not
		// be able to tell the two agents apart.
		var bodies []string
		for _, svc := range []interface {
			UpdateMockParams(context.Context, models.MockFilterParams) error
		}{&paramsSvc{}, &perTestSvc{}} {
			srv := serve(t, svc)
			res, err := http.Post(srv.URL+"/agent/updatemockparams", "application/json",
				strings.NewReader(`{"filterParams":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusOK {
				t.Fatalf("%T answered %d %q", svc, res.StatusCode, body)
			}
			bodies = append(bodies, string(body))
			_, isPerTest := svc.(*perTestSvc)
			if got := res.Header.Get(models.ConsumedScopeHeader); (got == models.ConsumedScopePerTest) != isPerTest {
				t.Fatalf("%T sent %s: %q", svc, models.ConsumedScopeHeader, got)
			}
		}
		if bodies[0] != bodies[1] {
			t.Fatalf("the answers' bodies differ:\n without the scope %q\n with it          %q", bodies[0], bodies[1])
		}
	})
}

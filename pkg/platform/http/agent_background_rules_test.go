package http_test

// A replay relaxes today's checks for a test set served by the background rules only
// when the agent's answer to the set's staging says it serves it by them, in
// the X-Keploy-Background-Rules header of its /updatemockparams answer. An
// agent released before background serving sends no header. These run the real routes
// against the real client.

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// backgroundSvc says it serves the staged set's background by rules.
type backgroundSvc struct {
	paramsSvc
	mu    sync.Mutex
	rules string
}

func (s *backgroundSvc) ServedBackground() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rules
}

func (s *backgroundSvc) set(rules string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = rules
}

func serveBackground(t *testing.T, svc *backgroundSvc) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestBackgroundRulesOnlyFromAStagingTheAgentAnswers(t *testing.T) {
	ctx := context.Background()
	stage := models.MockFilterParams{Exact: &models.ExactServing{Stage: true, Background: models.RecordedWithS12Routing}}
	open := models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}

	t.Run("an agent that says it serves the background rules is believed for its staging", func(t *testing.T) {
		svc := &backgroundSvc{rules: models.RecordedWithS12Routing}
		c := newClient(t, serveBackground(t, svc).URL+"/agent")
		if c.AgentServesBackground() != "" {
			t.Fatal("rules believed before any staging was answered")
		}
		if err := c.UpdateMockParams(ctx, stage); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		if got := c.AgentServesBackground(); got != models.RecordedWithS12Routing {
			t.Fatalf("the client took %q from the staging's answer; want %q", got, models.RecordedWithS12Routing)
		}
		// A test's opening says nothing of the rules, and changes nothing.
		svc.set("")
		if err := c.UpdateMockParams(ctx, open); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		if got := c.AgentServesBackground(); got != models.RecordedWithS12Routing {
			t.Fatalf("a test's opening changed the staged rules to %q", got)
		}
		// The next staging's answer, saying none, is today's.
		if err := c.UpdateMockParams(ctx, stage); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		if got := c.AgentServesBackground(); got != "" {
			t.Fatalf("a staging answered with no rules left %q", got)
		}
	})

	t.Run("an agent that predates the header serves today's rules", func(t *testing.T) {
		c := newClient(t, serve(t, &paramsSvc{}).URL+"/agent")
		if err := c.UpdateMockParams(ctx, stage); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		if got := c.AgentServesBackground(); got != "" {
			t.Fatalf("an agent that never said its rules was taken to serve %q", got)
		}
	})

	t.Run("a staging that fails believes nothing", func(t *testing.T) {
		svc := &backgroundSvc{rules: models.RecordedWithS12Routing}
		srv := serveBackground(t, svc)
		c := newClient(t, srv.URL+"/agent")
		if err := c.UpdateMockParams(ctx, stage); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
		svc.fail.Store(true)
		if err := c.UpdateMockParams(ctx, stage); err == nil {
			t.Fatal("precondition: the staging was supposed to fail")
		}
		if got := c.AgentServesBackground(); got != "" {
			t.Fatalf("a failed staging left the rules %q believed", got)
		}
	})
}

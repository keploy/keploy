package http_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// readsRequestSvc opens a test case in one step and says whether its
// OpenTestCase hands the learn step the test case's request.
type readsRequestSvc struct {
	paramsSvc
	reads bool
}

func (s *readsRequestSvc) OpenTestCase(ctx context.Context, p models.MockFilterParams) error {
	return s.UpdateMockParams(ctx, p)
}

func (s *readsRequestSvc) ReadsTestRequest() bool { return s.reads }

// The client believes the agent read a test case's request only from an
// answer that says so (models.TestRequestReadHeader), the real routes in
// front: every call clears it first, so a staging after it, a call that
// fails, and an agent that reads none leave it false.
func TestTheClientBelievesARequestReadOnlyFromAnAnswerThatSaysSo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	opening := models.MockFilterParams{AfterTime: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		TestRequest: &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "url", Recorded: "/a", Sent: "/a"}}}}
	staging := models.MockFilterParams{Exact: &models.ExactServing{Stage: true}}
	for _, reads := range []bool{true, false} {
		svc := &readsRequestSvc{reads: reads}
		r := chi.NewRouter()
		routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)
		c := newClient(t, srv.URL+"/agent")
		if err := c.UpdateMockParams(ctx, opening); err != nil {
			t.Fatal(err)
		}
		if got := c.AgentReadTestRequest(); got != reads {
			t.Fatalf("reads %v: after the opening filter the client believes %v", reads, got)
		}
		if err := c.UpdateMockParams(ctx, staging); err != nil {
			t.Fatal(err)
		}
		if c.AgentReadTestRequest() {
			t.Fatalf("reads %v: a staging's answer left the belief standing", reads)
		}
		if err := c.UpdateMockParams(ctx, opening); err != nil {
			t.Fatal(err)
		}
		svc.fail.Store(true)
		if err := c.UpdateMockParams(ctx, opening); err == nil {
			t.Fatal("a failing filter succeeded")
		}
		if c.AgentReadTestRequest() {
			t.Fatalf("reads %v: a failed call left the belief standing", reads)
		}
	}
}

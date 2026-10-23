package http_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
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

// The client says the agent read a test case's request only where the
// agent's answer to that very filter says so (models.TestRequestReadHeader),
// the real routes in front: an agent that reads none, a staging, and a call
// that fails say no; and what a filter's answer said is its own, whatever
// filters and stagings run beside it.
func TestTheClientSaysARequestReadOnlyFromItsOwnAnswer(t *testing.T) {
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
		if got, err := c.SendTestCaseFilter(ctx, opening); err != nil || got != reads {
			t.Fatalf("reads %v: the opening filter said %v (%v)", reads, got, err)
		}
		if got, err := c.SendTestCaseFilter(ctx, staging); err != nil || got {
			t.Fatalf("reads %v: a staging said %v (%v); want no request read", reads, got, err)
		}
		// Beside other filters and stagings, each says its own answer.
		var wg sync.WaitGroup
		bad := make(chan string, 400)
		for i := 0; i < 100; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if got, err := c.SendTestCaseFilter(ctx, opening); err != nil || got != reads {
					bad <- fmt.Sprintf("an opening said %v (%v)", got, err)
				}
			}()
			go func() {
				defer wg.Done()
				if err := c.UpdateMockParams(ctx, staging); err != nil {
					bad <- err.Error()
				}
			}()
		}
		wg.Wait()
		close(bad)
		for b := range bad {
			t.Fatalf("reads %v, beside stagings: %s", reads, b)
		}
		svc.fail.Store(true)
		if got, err := c.SendTestCaseFilter(ctx, opening); err == nil || got {
			t.Fatalf("reads %v: a failing filter said %v (%v)", reads, got, err)
		}
	}
}

// The client keeps the keploy version the agent's last answer to
// /updatemockparams or /mock named (models.AgentVersionHeader), the real
// routes in front, which name the agent's own on every answer: none before
// an answer, and none from an answer that names none, as an agent older than
// this keploy does not.
func TestTheClientKeepsTheVersionTheAgentNames(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := utils.Version
	t.Cleanup(func() { utils.Version = own })
	ctx := context.Background()
	opening := models.MockFilterParams{TestRequest: &models.TestRequest{Test: "test-1"}}
	send := map[string]func(t *testing.T) (call func() error, version func() string){
		"/updatemockparams": func(t *testing.T) (func() error, func() string) {
			r := chi.NewRouter()
			routes.DefaultRoutes{}.New(r, &readsRequestSvc{reads: true}, zap.NewNop())
			srv := httptest.NewServer(r)
			t.Cleanup(srv.Close)
			c := newClient(t, srv.URL+"/agent")
			return func() error { _, err := c.SendTestCaseFilter(ctx, opening); return err }, c.AgentVersion
		},
		"/mock": func(t *testing.T) (func() error, func() string) {
			r := chi.NewRouter()
			routes.DefaultRoutes{}.New(r, &clientTestsSvc{reads: true}, zap.NewNop())
			srv := httptest.NewServer(r)
			t.Cleanup(srv.Close)
			c := newClient(t, srv.URL+"/agent")
			return func() error { return c.MockOutgoing(ctx, models.OutgoingOptions{Mocking: true, Learn: true}) }, c.AgentVersion
		},
	}
	for name, open := range send {
		call, version := open(t)
		if v := version(); v != "" {
			t.Fatalf("%s: before any answer the client names the agent %q", name, v)
		}
		utils.Version = "v3.7.1"
		if err := call(); err != nil {
			t.Fatal(err)
		}
		if v := version(); v != "v3.7.1" {
			t.Fatalf("%s: the agent named v3.7.1; the client keeps %q", name, v)
		}
		utils.Version = ""
		if err := call(); err != nil {
			t.Fatal(err)
		}
		if v := version(); v != "" {
			t.Fatalf("%s: the agent's last answer named no version; the client keeps %q", name, v)
		}
	}
}

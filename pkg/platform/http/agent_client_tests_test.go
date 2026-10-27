package http_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// clientTestsSvc serves keploy mock's MockOutgoing and says whether its
// learn step reads the session's tests as client tests.
type clientTestsSvc struct {
	agentsvc.Service // nil: any other call panics loudly
	reads            bool
	fail             atomic.Bool
}

func (s *clientTestsSvc) MockOutgoing(context.Context, models.OutgoingOptions) error {
	if s.fail.Load() {
		return errors.New("no mock session")
	}
	return nil
}

func (s *clientTestsSvc) ReadsClientTests() bool { return s.reads }

// keploy mock's client believes the agent reads its tests as client tests
// only from a /mock answer that says so (models.ClientTestsReadHeader), the
// real routes in front: the answer says so only to a session that turns the
// learn step on, and every call clears the belief first, so a call that
// fails, and an agent that reads none, leave it false.
func TestKeployMocksClientBelievesClientTestsReadOnlyFromAnAnswerThatSaysSo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	for _, reads := range []bool{true, false} {
		svc := &clientTestsSvc{reads: reads}
		r := chi.NewRouter()
		routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)
		c := newClient(t, srv.URL+"/agent")
		if err := c.MockOutgoing(ctx, models.OutgoingOptions{Mocking: true, Learn: true}); err != nil {
			t.Fatal(err)
		}
		if got := c.AgentReadsClientTests(); got != reads {
			t.Fatalf("reads %v: after a learning session's MockOutgoing the client believes %v", reads, got)
		}
		if err := c.MockOutgoing(ctx, models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if c.AgentReadsClientTests() {
			t.Fatalf("reads %v: a session that learns nothing was answered as one whose tests are read as client tests", reads)
		}
		if err := c.MockOutgoing(ctx, models.OutgoingOptions{Mocking: true, Learn: true}); err != nil {
			t.Fatal(err)
		}
		svc.fail.Store(true)
		if err := c.MockOutgoing(ctx, models.OutgoingOptions{Mocking: true, Learn: true}); err == nil {
			t.Fatal("a failing MockOutgoing succeeded")
		}
		if c.AgentReadsClientTests() {
			t.Fatalf("reads %v: a failed call left the belief standing", reads)
		}
	}
}

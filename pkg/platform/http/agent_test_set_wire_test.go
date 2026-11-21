package http_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// testSetSvc keeps the test set each MockOutgoing reached the agent's
// service under (models.TestSetSessionOf its ctx).
type testSetSvc struct {
	agentsvc.Service // nil: any other call panics loudly
	mu               sync.Mutex
	got              []models.TestSetSession
}

func (s *testSetSvc) MockOutgoing(ctx context.Context, _ models.OutgoingOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, models.TestSetSessionOf(ctx))
	return nil
}

// The test set keploy test starts a session for reaches the agent's service
// over the real wire: the client's MockOutgoing sends what its ctx names
// (models.OutgoingReq.TestSet), and the agent's /mock route puts it in the
// service's ctx. A ctx that names none (a record, keploy mock, an older
// keploy test) reaches it naming none.
func TestTheTestSetASessionServesReachesTheAgent(t *testing.T) {
	t.Parallel()
	svc := &testSetSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL+"/agent")

	set := models.TestSetSession{Run: "run-a", Set: "test-set-1"}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want models.TestSetSession
	}{
		{"keploy test's set", models.WithTestSetSession(context.Background(), set), set},
		{"no set", context.Background(), models.TestSetSession{}},
	} {
		if err := c.MockOutgoing(tc.ctx, models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatalf("%s: MockOutgoing: %v", tc.name, err)
		}
		svc.mu.Lock()
		got := svc.got
		svc.got = nil
		svc.mu.Unlock()
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: the agent's service got %+v, want one call naming %+v", tc.name, got, tc.want)
		}
	}
}

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

// learnTestsSvc keeps the options keploy mock's MockOutgoing reached the
// agent's service with.
type learnTestsSvc struct {
	agentsvc.Service // nil: any other call panics loudly
	mu               sync.Mutex
	got              []models.OutgoingOptions
}

func (s *learnTestsSvc) MockOutgoing(_ context.Context, opts models.OutgoingOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, opts)
	return nil
}

// LearnTests reaches the agent's service as keploy mock set it, over the
// real wire: the client's MockOutgoing encodes it, the agent's /mock route
// decodes it. An empty list (a round that learns onto no test) stays an
// empty list, which the agent reads as "no test learns"; nil (a CLI that
// learns onto every test's recording) stays nil. Read as nil, an empty list
// would make every test of the round fresh to the agent, and a carried test
// begun after a newer one would stop the round's learning.
func TestLearnTestsReachTheAgentAsKeployMockSetThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &learnTestsSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL+"/agent")

	for _, tc := range []struct {
		name string
		in   map[string]bool
	}{
		{"no test", map[string]bool{}},
		{"every test", nil},
		{"one test", map[string]bool{"o::TestA": true}},
	} {
		if err := c.MockOutgoing(ctx, models.OutgoingOptions{Mocking: true, Learn: true, LearnTests: tc.in}); err != nil {
			t.Fatalf("%s: MockOutgoing: %v", tc.name, err)
		}
		svc.mu.Lock()
		if len(svc.got) == 0 {
			svc.mu.Unlock()
			t.Fatalf("%s: the agent's service was never called", tc.name)
		}
		got := svc.got[len(svc.got)-1]
		svc.got = nil
		svc.mu.Unlock()
		if !got.Learn {
			t.Fatalf("%s: Learn did not reach the agent", tc.name)
		}
		if (got.LearnTests == nil) != (tc.in == nil) || len(got.LearnTests) != len(tc.in) {
			t.Fatalf("%s: LearnTests %#v reached the agent as %#v", tc.name, tc.in, got.LearnTests)
		}
		for name := range tc.in {
			if !got.LearnTests[name] {
				t.Fatalf("%s: %s did not reach the agent", tc.name, name)
			}
		}
	}
}

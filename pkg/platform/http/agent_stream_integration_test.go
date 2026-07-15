package http_test

// End-to-end streaming test: a real chi router + real agent route handlers
// (DefaultRoutes) wired to a stub Service, behind an httptest server, driven by
// the real AgentClient. Proves the client streams the corpus and the real
// /storemocks handler decodes the header and drives StoreMocksStream. Streaming
// is the default wire format; StoreMocks falls back to the legacy single-shot
// framing only when an older agent rejects the stream with 400 (covered in
// agent_fallback_test.go).

import (
	"context"
	"encoding/gob"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// stubSvc implements just enough of agent.Service for the /storemocks paths.
// The embedded nil interface satisfies the type; unexercised methods would
// panic if hit (they aren't in this test).
type stubSvc struct {
	agentsvc.Service
	mu          sync.Mutex
	legacyCalls int
	streamCalls int
	filtered    int
	unfiltered  int
}

func (s *stubSvc) StoreMocks(_ context.Context, f, u []*models.Mock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacyCalls++
	s.filtered, s.unfiltered = len(f), len(u)
	return nil
}

// StoreMocksStream is the capability method the handler discovers via type
// assertion. It fully drains the stream (validating framing) before recording.
func (s *stubSvc) StoreMocksStream(_ context.Context, h models.MockStreamHeader, dec *gob.Decoder) error {
	total := h.FilteredCount + h.UnfilteredCount
	for i := 0; i < total; i++ {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamCalls++
	s.filtered, s.unfiltered = h.FilteredCount, h.UnfilteredCount
	return nil
}

func newClient(t *testing.T, agentURI string) *httpclient.AgentClient {
	t.Helper()
	return httpclient.New(zap.NewNop(), nil, &config.Config{
		Agent: config.Agent{SetupOptions: models.SetupOptions{AgentURI: agentURI}},
	})
}

func fixtures() (filtered, unfiltered []*models.Mock) {
	return []*models.Mock{{Name: "f1", Kind: models.HTTP}, {Name: "f2", Kind: models.Mongo}},
		[]*models.Mock{{Name: "u1", Kind: models.DNS}}
}

// End-to-end: the client streams to the real agent routes, which decode the
// header and drive StoreMocksStream. (Streaming is the default path; the legacy
// single-shot fallback for older agents is covered in agent_fallback_test.go.)
func TestStoreMocks_StreamsToAgent(t *testing.T) {
	svc := &stubSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	f, u := fixtures()
	if err := client.StoreMocks(context.Background(), f, u); err != nil {
		t.Fatalf("StoreMocks: %v", err)
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.streamCalls != 1 || svc.legacyCalls != 0 {
		t.Fatalf("expected 1 stream call, 0 legacy; got stream=%d legacy=%d", svc.streamCalls, svc.legacyCalls)
	}
	if svc.filtered != len(f) || svc.unfiltered != len(u) {
		t.Fatalf("counts mismatch: got f=%d u=%d want f=%d u=%d", svc.filtered, svc.unfiltered, len(f), len(u))
	}
}

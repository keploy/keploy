package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// An agent docker compose starts again behind keploy test's back is told the
// session's options again, and with them the test set the session serves
// (models.OutgoingReq.TestSet): a loss the restarted agent finds after that
// set's end is then still named as that set's, not as a session of no set's.
func TestARestartedAgentIsToldTheTestSetItsSessionServes(t *testing.T) {
	var mu sync.Mutex
	var got []models.TestSetSession
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.OutgoingReq
		if r.URL.Path != "/mock" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		got = append(got, req.TestSet)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
	}))
	defer srv.Close()
	s := storeSession(t, srv.URL, true)
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	set := models.TestSetSession{Run: "run-a", Set: "test-set-1"}
	if err := a.MockOutgoing(models.WithTestSetSession(context.Background(), set), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != set || got[1] != set {
		t.Fatalf("the agent, then the restarted one, were told the sets %+v; want %+v both times", got, set)
	}
}

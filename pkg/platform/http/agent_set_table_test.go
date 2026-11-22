package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestRestartedAgentGetsTheSetTableAgain(t *testing.T) {
	var mu sync.Mutex
	var got []models.ScopeTableReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.ScopeTableReq
		if r.URL.Path != "/scope/table" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
	}))
	defer srv.Close()
	s := storeSession(t, srv.URL, true)
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	start := time.Date(2026, 10, 12, 1, 6, 31, 0, time.UTC)
	sets := map[string]models.SetTable{"e2e/orders": {Tests: map[string][]models.Owned{"T": {{Name: "mock-1"}}}, Start: start}}
	if err := a.PushSetTable(context.Background(), "/repo", sets); err != nil {
		t.Fatal(err)
	}
	delete(sets, "e2e/orders")
	if err := s.reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the agent got the set table %d times, want 2", len(got))
	}
	again := got[1]
	if again.Root != "/repo" || !again.Sets["e2e/orders"].Start.Equal(start) || len(again.Sets["e2e/orders"].Tests["T"]) != 1 {
		t.Fatalf("the restarted agent got %+v", again)
	}
}

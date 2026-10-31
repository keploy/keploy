package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// PushTestBegins posts the tests' recorded begins to /replay/clock; an agent
// that does not take them is an error, never a silent success, because the
// tests would run on another clock.
func TestAgentClient_PushTestBegins(t *testing.T) {
	var got models.TestBeginsReq
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/replay/clock" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()
	at := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)

	if err := a.PushTestBegins(ctx, models.TestBeginsReq{Begins: map[string]time.Time{"pkg::TestA": at}}); err != nil {
		t.Fatal(err)
	}
	if !got.Begins["pkg::TestA"].Equal(at) {
		t.Fatalf("the agent got %+v", got)
	}
	for _, s := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusInternalServerError} {
		status = s
		err := a.PushTestBegins(ctx, models.TestBeginsReq{})
		if err == nil || !strings.Contains(err.Error(), "did not take") {
			t.Fatalf("status %d: want an error, got %v", s, err)
		}
	}
}

// A restarted agent is given the tests' recorded begins and incoming requests
// again, as the first one was: its tests run on their recorded clocks too, and
// it reads what the harness gave the application as the first one did.
func TestRestartedAgentGetsTheTestBeginsAgain(t *testing.T) {
	var mu sync.Mutex
	var got []models.TestBeginsReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.TestBeginsReq
		if r.URL.Path != "/replay/clock" || json.NewDecoder(r.Body).Decode(&req) != nil {
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
	at := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	begins := map[string]time.Time{"pkg::TestA": at}
	incoming := map[string][]models.TestIncoming{"pkg::TestA": {{Case: "test-1", At: at, URL: "http://app/v1/x", Header: map[string]string{"X-Tenant": "tn-1"}}}}
	if err := a.PushTestBegins(context.Background(), models.TestBeginsReq{Begins: begins, Incoming: incoming}); err != nil {
		t.Fatal(err)
	}
	delete(begins, "pkg::TestA")
	delete(incoming, "pkg::TestA")
	if err := s.reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the agent got the begins %d times, want 2", len(got))
	}
	if !got[1].Begins["pkg::TestA"].Equal(at) {
		t.Fatalf("the restarted agent got %+v", got[1])
	}
	// The test's incoming requests too: without them the restarted agent
	// finds every variable the harness gave the application unbound.
	if in := got[1].Incoming["pkg::TestA"]; len(in) != 1 || in[0].Case != "test-1" || in[0].Header["X-Tenant"] != "tn-1" {
		t.Fatalf("the restarted agent got the incoming requests %+v", got[1].Incoming)
	}
}

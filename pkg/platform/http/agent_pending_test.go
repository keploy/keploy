package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// PendingBefore asks the agent's /record/pending, with the time in Unix
// nanoseconds, and reads its answer; an agent that cannot tell (501), or
// predates the route (404), is known not to.
func TestAgentClient_PendingBefore(t *testing.T) {
	var status int
	var body, gotBefore string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/record/pending" {
			http.NotFound(w, r)
			return
		}
		gotBefore = r.URL.Query().Get("before")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	before := time.Unix(0, 1234567890)
	for _, tc := range []struct {
		status         int
		body           string
		pending, known bool
	}{
		{http.StatusOK, `{"pending":true}`, true, true},
		{http.StatusOK, `{"pending":false}`, false, true},
		{http.StatusNotImplemented, "cannot tell", false, false},
		{http.StatusNotFound, "", false, false},
	} {
		status, body = tc.status, tc.body
		pending, known, err := a.PendingBefore(context.Background(), before)
		if err != nil || pending != tc.pending || known != tc.known {
			t.Errorf("status %d: (%v, %v, %v), want (%v, %v)", tc.status, pending, known, err, tc.pending, tc.known)
		}
	}
	if gotBefore != "1234567890" {
		t.Fatalf("asked before=%q, want the stop in Unix nanoseconds", gotBefore)
	}
}

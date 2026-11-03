package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
)

// NotifyStopBegun posts to the agent's /stop-begun and errors unless the agent
// took it; with no agent it does nothing.
func TestNotifyStopBegunErrorsUnlessTheAgentTookIt(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotImplemented} {
		var path atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path.Store(r.Method + " " + r.URL.Path)
			w.WriteHeader(status)
		}))
		a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL + "/agent"}}}
		err := a.NotifyStopBegun(context.Background())
		srv.Close()
		if got, _ := path.Load().(string); got != "POST /agent/stop-begun" {
			t.Fatalf("the agent was asked %q, want POST /agent/stop-begun", got)
		}
		if (err == nil) != (status == http.StatusOK) {
			t.Fatalf("the agent answered %d: NotifyStopBegun = %v", status, err)
		}
	}
	none := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{}}
	if err := none.NotifyStopBegun(context.Background()); err != nil {
		t.Fatalf("with no agent: %v", err)
	}
}

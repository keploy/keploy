package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// BeginTestErrorCapture asks the agent to open a test's window;
// ContinueTestErrorCapture asks it to carry in what was missed since the
// previous one closed (?carry=1). An agent that predates the route (404) is a
// no-op for both.
func TestAgentClient_OpensTheTestsCaptureWindow(t *testing.T) {
	var asked []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/test-capture/begin" {
			http.NotFound(w, r)
			return
		}
		asked = append(asked, r.URL.RawQuery)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}

	if err := a.BeginTestErrorCapture(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.ContinueTestErrorCapture(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || asked[0] != "" || asked[1] != "carry=1" {
		t.Fatalf("asked with queries %q; want [\"\" \"carry=1\"]", asked)
	}
	status = http.StatusNotFound
	if err := a.ContinueTestErrorCapture(context.Background()); err != nil {
		t.Fatalf("an agent without the route: %v; want a no-op", err)
	}
}

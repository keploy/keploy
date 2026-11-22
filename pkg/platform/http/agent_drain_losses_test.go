package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// DrainCaptureLosses POSTs /capturelosses/drain and is nil once the agent
// answers that its proxy counted every close the application's stop made
// (204). An agent older than the route (404, 405) or one that does not drain
// (501) is ErrCaptureLossesUnsupported: the set's reads are as they were. Any
// other answer, a 200 among them, is an error naming the agent's: what the
// stop's closes lost cannot be told.
func TestAgentClient_DrainsTheClosesTheApplicationsStopMade(t *testing.T) {
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/capturelosses/drain" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			_, _ = w.Write([]byte(`{"error":"keploy's proxy had not finished counting the close of 1 connection(s)"}`))
		}
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}

	if err := a.DrainCaptureLosses(context.Background()); err != nil {
		t.Fatalf("204: %v, want nil", err)
	}
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		status = code
		if err := a.DrainCaptureLosses(context.Background()); !errors.Is(err, models.ErrCaptureLossesUnsupported) {
			t.Fatalf("%d: %v, want ErrCaptureLossesUnsupported", code, err)
		}
	}
	for _, code := range []int{http.StatusInternalServerError, http.StatusOK} {
		status = code
		err := a.DrainCaptureLosses(context.Background())
		if err == nil || errors.Is(err, models.ErrCaptureLossesUnsupported) {
			t.Fatalf("%d: %v, want an error that is not ErrCaptureLossesUnsupported", code, err)
		}
		if code == http.StatusInternalServerError && !strings.Contains(err.Error(), "had not finished counting") {
			t.Fatalf("500: %v, want the agent's reason", err)
		}
	}
}

package replay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// uriWatchedInstr is the compose harness's agent, its start watched as
// keploy watches an agent another process started, known only by its URI
// (a Kubernetes sidecar): the client's own watch (AgentClient.
// WatchAgentStart).
type uriWatchedInstr struct {
	*composeAgentInstr
}

func (u *uriWatchedInstr) WatchAgentStart(ctx context.Context) <-chan error {
	return u.client.WatchAgentStart(ctx)
}

// keploy test's per-set wait for an agent known only by its URI waits for
// one whose health check answers, not healthy, for a while (a forward in
// front of it, or an agent still starting), and then healthy: the set runs
// its tests. The watch's bound counts from the latest change in what its
// health check answers, so a late healthy answer within it is no failure.
func TestRunTestSet_ComposeRunsOnAnAgentThatAnswersHealthyLate(t *testing.T) {
	agent := &composeAgent{}
	router := chi.NewRouter()
	routes.DefaultRoutes{}.New(router, agent, zap.NewNop())
	healthyAt := time.Now().Add(1500 * time.Millisecond)
	var unhealthy atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/health" && time.Now().Before(healthyAt) {
			unhealthy.Add(1)
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer srv.Close()

	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Agent.AgentURI = srv.URL + "/agent"
	h.replayer.instrumentation = &uriWatchedInstr{&composeAgentInstr{prInstr: h.instr, client: httpclient.New(zap.NewNop(), nil, h.replayer.config)}}

	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if err != nil || status != models.TestSetStatusPassed {
		t.Fatalf("RunTestSet = (%s, %v); want PASSED on an agent that answered healthy late", status, err)
	}
	if unhealthy.Load() == 0 {
		t.Fatal("the agent's health check was never asked while it answered not healthy: the row proves nothing")
	}
	if len(h.report.results) != 2 {
		t.Fatalf("ran %d tests, want 2", len(h.report.results))
	}
}

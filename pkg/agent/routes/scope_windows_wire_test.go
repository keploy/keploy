package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// refusedWindowSvc answers every window read with one test keploy refused
// at its begin.
type refusedWindowSvc struct{ agent.Service }

const refusedWhy = "t1 began with the pid 4242: keploy narrows a test to its own mocks by the pid of its test process only in a run recorded with the suite mark or an app-start mark"

func (refusedWindowSvc) GetScopeWindows(context.Context) ([]models.ScopeWindow, error) {
	return []models.ScopeWindow{{Name: "t1", PID: 4242, Outcome: models.ScopeOutcomeFailed, BeginRefused: refusedWhy}}, nil
}

func (s refusedWindowSvc) FinalScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	return s.GetScopeWindows(ctx)
}

// The replay fails a test keploy refused at its begin by its window's
// BeginRefused, so the refusal must cross the agent's wire: the agent's
// answer to a windows read, read by the CLI's client, still names the
// refused test and why, with the switch off and on. Without it the replay
// passes on the test command's exit, a test refused or not.
func TestARefusedBeginCrossesTheWindowsWire(t *testing.T) {
	a := &Agent{logger: zap.NewNop(), svc: refusedWindowSvc{}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleScopeWindows))
	t.Cleanup(srv.Close)
	client := agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	for _, on := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, on)
		got, err := client.GetScopeWindows(context.Background())
		if err != nil {
			t.Fatalf("switch %q: %v", on, err)
		}
		if len(got) != 1 || got[0].Name != "t1" || got[0].PID != 4242 || got[0].BeginRefused != refusedWhy {
			t.Fatalf("switch %q: the CLI read %+v, want t1 (pid 4242) refused at its begin, saying why", on, got)
		}
	}
}

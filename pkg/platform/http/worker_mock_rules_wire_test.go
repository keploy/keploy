package http_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/provenswitch"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// rulesSvc is an agent service that records the worker-mock rules each
// /outgoing request sets before its stream opens.
type rulesSvc struct {
	agentsvc.Service // nil: any other call panics loudly
	set              [][]models.WorkerMockRule
	order            []string
}

func (s *rulesSvc) SetBackgroundRules(rules []models.WorkerMockRule) {
	s.set = append(s.set, rules)
	s.order = append(s.order, "rules")
}

func (s *rulesSvc) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	s.order = append(s.order, "outgoing")
	ch := make(chan *models.Mock)
	close(ch)
	return ch, nil
}

// The recording's worker-mock rules (config record.workerMocks) reach the
// agent's manager on the /outgoing request, before its stream opens, from the
// config the agent client runs with (k8s-proxy fills it from the stored
// rules). They travel beside OutgoingOptions, which every connection's
// supervisor.Session holds by value. Under the proven switch none are sent,
// and a recording with none clears the last one's.
func TestTheRecordingsWorkerMockRulesReachTheAgent(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	svc := &rulesSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	rule := models.WorkerMockRule{ID: "timer", Kind: "Mongo", Match: map[string]string{"command": "update", "collection": "jobs"}}
	record := func(rules []models.WorkerMockRule) {
		t.Helper()
		cfg := &config.Config{Agent: config.Agent{SetupOptions: models.SetupOptions{AgentURI: srv.URL + "/agent"}}}
		cfg.Record.WorkerMocks = rules
		c := httpclient.New(zap.NewNop(), nil, cfg)
		grp, gctx := errgroup.WithContext(context.Background())
		ctx := context.WithValue(gctx, models.ErrGroupKey, grp)
		ch, err := c.GetOutgoing(ctx, models.OutgoingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}

	record([]models.WorkerMockRule{rule})
	if len(svc.set) != 1 || len(svc.set[0]) != 1 || svc.set[0][0].ID != "timer" {
		t.Fatalf("the agent was set rules %v, want the recording's one rule", svc.set)
	}
	if svc.order[0] != "rules" || svc.order[1] != "outgoing" {
		t.Fatalf("calls %v: the rules must be set before the recording's stream opens", svc.order)
	}

	record(nil)
	if len(svc.set) != 2 || len(svc.set[1]) != 0 {
		t.Fatalf("a recording with no rules set %v, want them cleared", svc.set)
	}

	t.Setenv(provenswitch.Env, "1")
	record([]models.WorkerMockRule{rule})
	if len(svc.set) != 3 || len(svc.set[2]) != 0 {
		t.Fatalf("under the proven switch the agent was set %v, want none", svc.set)
	}
}

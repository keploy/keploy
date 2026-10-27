package replay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A replacement compose agent whose proven switch is not this keploy's must
// stop the run: re-registering mocks on it would replay on different paths
// than the verdicts. Any other failure to mark it ready stays the soft
// failure it has always been.
func TestEnsureAgentHoldsStoredMocks_RefusesAnAgentWithAnotherProvenSwitch(t *testing.T) {
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	f := &rearmInstr{storeTakesEffect: true, readyErr: fmt.Errorf("%w: the switch is on in this keploy and off in its agent", provenswitch.ErrAgentSwitch)}
	err := newRearmReplayer(f).ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases)
	if !errors.Is(err, provenswitch.ErrAgentSwitch) {
		t.Fatalf("ensureAgentHoldsStoredMocks = %v; an agent with another proven switch must stop the run", err)
	}

	g := &rearmInstr{storeTakesEffect: true, readyErr: errors.New("readiness file not written")}
	if err := newRearmReplayer(g).ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("a plain readiness failure must stay soft, got: %v", err)
	}
}

// refusingReadyInstr is the compose harness's agent, answering the readiness
// step the way the client does when the agent's proven switch is not keploy's.
type refusingReadyInstr struct {
	*composeAgentInstr
}

func (*refusingReadyInstr) MakeAgentReadyForDockerCompose(context.Context) error {
	return fmt.Errorf("%w: the switch is on in this keploy and off in its agent", provenswitch.ErrAgentSwitch)
}

// Under docker compose, a set whose agent has another proven switch must not
// run: no verdict of it could be trusted. Today a readiness failure is logged
// and the set runs; this one stops it.
func TestRunTestSet_ComposeRefusesAnAgentWithAnotherProvenSwitch(t *testing.T) {
	agent := &composeAgent{}
	router := chi.NewRouter()
	routes.DefaultRoutes{}.New(router, agent, zap.NewNop())
	srv := httptest.NewServer(router)
	defer srv.Close()

	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Agent.AgentURI = srv.URL + "/agent"
	h.replayer.instrumentation = &refusingReadyInstr{&composeAgentInstr{prInstr: h.instr, client: httpclient.New(zap.NewNop(), nil, h.replayer.config)}}

	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if !errors.Is(err, provenswitch.ErrAgentSwitch) || status != models.TestSetStatusInternalErr {
		t.Fatalf("RunTestSet = (%s, %v); want INTERNAL_ERR and an error wrapping provenswitch.ErrAgentSwitch", status, err)
	}
	if len(h.report.results) != 0 {
		t.Fatalf("ran %d tests against an agent with another proven switch", len(h.report.results))
	}
}

// stalledStartInstr is the compose harness's agent, whose start the watch
// gives up on (pkg.AgentStartWatcher).
type stalledStartInstr struct {
	*composeAgentInstr
}

func (*stalledStartInstr) WatchAgentStart(context.Context) <-chan error {
	out := make(chan error, 1)
	out <- fmt.Errorf("%w: its start took no step across 30 of its beats", pkg.ErrAgentNotReady)
	return out
}

// keploy test's per-set wait for the agent docker compose starts follows the
// agent's start: a start that stopped fails the set, saying so, where its
// health check never answers; no budget of wall time does.
func TestRunTestSet_ComposeEndsOnTheAgentsStalledStart(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uri := "http://" + closed.Addr().String() + "/agent"
	_ = closed.Close()
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Agent.AgentURI = uri
	h.replayer.instrumentation = &stalledStartInstr{&composeAgentInstr{prInstr: h.instr, client: httpclient.New(zap.NewNop(), nil, h.replayer.config)}}

	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "did not come up") || status != models.TestSetStatusFailed {
		t.Fatalf("RunTestSet = (%s, %v); want FAILED and the agent's stalled start", status, err)
	}
	if len(h.report.results) != 0 {
		t.Fatalf("ran %d tests on an agent that never came up", len(h.report.results))
	}
}

package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// readyInstr is setupInstr with a chosen answer to MakeAgentReadyForDockerCompose.
type readyInstr struct {
	setupInstr
	readyErr error
}

func (r *readyInstr) MakeAgentReadyForDockerCompose(context.Context) error { return r.readyErr }

// The runner must not set up a test set on an agent whose proven switch is not
// this keploy's; any other failure to mark it ready stays the soft failure it
// has always been.
func TestSetupTestSetRefusesAnAgentWithAnotherProvenSwitch(t *testing.T) {
	run := func(readyErr error) error {
		r := &Runner{
			logger:          zap.NewNop(),
			instrumentation: &readyInstr{readyErr: readyErr},
			mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
			mockDB:          &skippingMockDB{},
		}
		setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
		if err == nil {
			setup.cleanup()
		}
		return err
	}
	if err := run(fmt.Errorf("%w: the switch is on in this keploy and off in its agent", provenswitch.ErrAgentSwitch)); !errors.Is(err, provenswitch.ErrAgentSwitch) {
		t.Fatalf("setupTestSet = %v; an agent with another proven switch must stop the set", err)
	}
	if err := run(errors.New("readiness file not written")); err != nil {
		t.Fatalf("a plain readiness failure must stay soft, got: %v", err)
	}
}

// startWatchInstr is setupInstr whose agent's start the watch gives up on.
type startWatchInstr struct {
	setupInstr
	watched int
}

func (*startWatchInstr) Setup(context.Context, string, models.SetupOptions) error { return nil }

func (s *startWatchInstr) WatchAgentStart(context.Context) <-chan error {
	s.watched++
	out := make(chan error, 1)
	out <- fmt.Errorf("%w: its start took no step across 30 of its beats", pkg.ErrAgentNotReady)
	return out
}

// The runner's gate on its agent follows the agent's start, through the
// instrumentation's own watch (a wrapper passes it on: the interface has it):
// a start that stopped ends it with that, where the health check never
// answers; no budget of wall time does.
func TestSetupTestSetEndsOnTheAgentsStalledStart(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uri := "http://" + closed.Addr().String() + "/agent"
	_ = closed.Close()
	instr := &startWatchInstr{}
	r := &Runner{
		logger:          zap.NewNop(),
		instrumentation: instr,
		config:          &config.Config{Agent: config.Agent{AgentURI: uri}},
		mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
		mockDB:          &skippingMockDB{},
	}
	setup, err := r.setupTestSet(context.Background(), "test-set-0", time.Time{})
	if err == nil {
		setup.cleanup()
	}
	if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "did not come up") || instr.watched != 1 {
		t.Fatalf("setupTestSet = %v after %d watch(es); want the agent's stalled start, watched once", err, instr.watched)
	}
}

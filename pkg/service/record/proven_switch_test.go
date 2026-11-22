package record

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// refusingComposeInstr is the compose agent of the record tests, answering the
// readiness step the way the client does when the agent's proven switch is
// not keploy's.
type refusingComposeInstr struct {
	*composeInstr
}

func (*refusingComposeInstr) MakeAgentReadyForDockerCompose(context.Context) error {
	return fmt.Errorf("%w: the switch is on in this keploy and off in its agent", provenswitch.ErrAgentSwitch)
}

// Under docker compose, recording against an agent whose proven switch is not
// keploy's would capture under the wrong rules: the recording stops. Any
// other readiness failure stays the logged soft failure it was.
func TestStart_ComposeRefusesAnAgentWithAnotherProvenSwitch(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &refusingComposeInstr{&composeInstr{fakeInstr: f, stackDown: make(chan struct{})}}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer agent.Close()
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	cfg.Agent.AgentURI = agent.URL
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          cfg,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, provenswitch.ErrAgentSwitch) {
			t.Fatalf("Start = %v; want the recording stopped with an error wrapping provenswitch.ErrAgentSwitch", err)
		}
	case <-time.After(60 * time.Second): // a harness that never returns must not hang the suite
		cancel()
		t.Fatal("Start kept recording against an agent with another proven switch")
	}
}

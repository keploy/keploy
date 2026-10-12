package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

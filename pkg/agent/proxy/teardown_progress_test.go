package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// busyCheckingHook is an auxiliary hook whose teardown notes whether the
// agent's teardown reported it as work in progress while it ran.
type busyCheckingHook struct {
	sawBusy, ran bool
}

func (h *busyCheckingHook) AfterStart(context.Context, agent.Proxy) error { return nil }

func (h *busyCheckingHook) BeforeTeardown(context.Context) error {
	h.ran = true
	h.sawBusy = utils.TeardownBusy()
	return nil
}

// The auxiliary hook's teardown -- where an enterprise agent removes its TLS
// uprobes -- runs as teardown work in progress (utils.BeginTeardownWork): the
// CLI that stopped the agent takes no check made during it for one without
// progress, and so never kills the agent part way through a removal, which
// would leak the uprobe. The work ends with it.
func TestTheHooksTeardownIsWorkInProgress(t *testing.T) {
	h := &busyCheckingHook{}
	p := &Proxy{logger: zap.NewNop(), auxiliaryHook: h}
	before := utils.TeardownSteps()
	p.detachThenRemoveCA(context.Background())
	if !h.ran {
		t.Fatal("the auxiliary hook's teardown did not run")
	}
	if !h.sawBusy {
		t.Fatal("the auxiliary hook's teardown ran without being reported as work in progress")
	}
	if utils.TeardownBusy() {
		t.Fatal("the teardown is still reported as work in progress after it")
	}
	if steps := utils.TeardownSteps() - before; steps < 4 {
		t.Fatalf("the hook's teardown and the CA's removal recorded %d steps, want a start and an end each", steps)
	}
}

package proxy

import (
	"context"
	"testing"
	"time"

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
	if steps := utils.TeardownSteps() - before; steps < 3 {
		t.Fatalf("the hook's teardown and the CA's removal recorded %d steps, want the work's start, its end, and one between them", steps)
	}
}

// The proxy's shutdown records its stages as steps of the agent's teardown,
// and the detach and the CA's removal as work in progress, which the CLI that
// stopped the agent reads: a teardown that records none is taken for one that
// has stopped making progress. Driven through start(), as the agent stops it.
func TestTheProxysShutdownRecordsItsSteps(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), skipListener: true}
	ctx, cancel := context.WithCancel(context.Background())
	readyChan := make(chan error, 1)
	startErr := make(chan error, 1)
	go func() { startErr <- p.start(ctx, readyChan) }()
	select {
	case err := <-readyChan:
		if err != nil {
			cancel()
			t.Fatalf("start() reported not-ready: %v", err)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		cancel()
		t.Fatal("start() never signalled readiness")
	}
	before := utils.TeardownSteps()
	cancel()
	select {
	case <-startErr:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("start() did not return after its context was cancelled")
	}
	// Two stages, then the detach and the CA's removal: begun, between, ended.
	if steps := utils.TeardownSteps() - before; steps < 5 {
		t.Fatalf("the proxy's shutdown recorded %d teardown steps, want at least 5", steps)
	}
	if utils.TeardownBusy() {
		t.Fatal("the proxy's shutdown left teardown work in progress")
	}
}

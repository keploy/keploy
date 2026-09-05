package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The native agent's half of replay.ConsumerInstrumentation.
//
// `keploy test` reaches the gate over HTTP: the replayer holds an
// *http.AgentClient, the client calls /agent/consumer/*, the routes call THESE
// methods, and these methods reach the proxy that owns the gate the parsers
// were handed on their contexts. Every link is load-bearing, and each one was
// separately deletable with the suite green.

// gateProxy is a Proxy with the consumer capability.
type gateProxy struct {
	coreAgent.Proxy
	armed    []models.ConsumerArm
	awaited  []string
	resetFor []string
	armErr   error
	res      models.ConsumerResult
	trailing int
}

func (g *gateProxy) ArmConsumerTrigger(_ context.Context, arm models.ConsumerArm) error {
	g.armed = append(g.armed, arm)
	return g.armErr
}

func (g *gateProxy) AwaitConsumerEffects(_ context.Context, testID string) (*models.ConsumerResult, error) {
	g.awaited = append(g.awaited, testID)
	res := g.res
	res.TestID = testID
	return &res, nil
}

func (g *gateProxy) ResetConsumerGate(_ context.Context, testSetID string) (int, error) {
	g.resetFor = append(g.resetFor, testSetID)
	return g.trailing, nil
}

// plainProxy is a Proxy without it — an embedder's own proxy, or an older one.
type plainProxy struct{ coreAgent.Proxy }

func agentWith(p coreAgent.Proxy) *Agent {
	return &Agent{logger: zap.NewNop(), Proxy: p}
}

func TestTheAgentDelegatesTheConsumerWindowToItsProxy(t *testing.T) {
	p := &gateProxy{res: models.ConsumerResult{
		TriggerAccepted: true,
		ObservedEffects: 2,
		EndReason:       models.ConsumerEndReasonCountReached,
	}}
	a := agentWith(p)

	arm := models.ConsumerArm{TestID: "test-7", TestSetID: "test-set-0", Protocol: "fake"}
	if err := a.ArmConsumerTrigger(context.Background(), arm); err != nil {
		t.Fatalf("ArmConsumerTrigger: %v", err)
	}
	if len(p.armed) != 1 || p.armed[0].TestID != "test-7" {
		t.Fatalf("the arm did not reach the proxy: %+v", p.armed)
	}

	res, err := a.AwaitConsumerEffects(context.Background(), "test-7")
	if err != nil {
		t.Fatalf("AwaitConsumerEffects: %v", err)
	}
	if res.TestID != "test-7" || res.EndReason != models.ConsumerEndReasonCountReached {
		t.Fatalf("the window did not come back: %+v", res)
	}

	trailing, err := a.ResetConsumerGate(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("ResetConsumerGate: %v", err)
	}
	if trailing != 0 {
		t.Fatalf("trailing = %d, want the proxy's own count", trailing)
	}
	if len(p.resetFor) != 1 || p.resetFor[0] != "test-set-0" {
		t.Fatalf("the reset did not reach the proxy: %v", p.resetFor)
	}
}

// A proxy that cannot open a delivery window is REFUSED, never degraded: it
// cannot deliver the recorded message at all, so the worker produces nothing
// and the test would otherwise report "the worker stopped producing" — blaming
// the application for a missing capability in keploy.
func TestAProxyWithoutTheCapabilityIsRefusedByName(t *testing.T) {
	a := agentWith(&plainProxy{})

	err := a.ArmConsumerTrigger(context.Background(), models.ConsumerArm{TestID: "test-1"})
	if err == nil {
		t.Fatal("arming a proxy that has no delivery gate must fail")
	}
	if !strings.Contains(err.Error(), string(models.CategoryConsumerUnsupportedAgent)) {
		t.Fatalf("the refusal must be named, got %v", err)
	}
	if _, err := a.AwaitConsumerEffects(context.Background(), "test-1"); err == nil {
		t.Fatal("awaiting a proxy that has no delivery gate must fail")
	}

	// The reset is the one that must stay silent: a proxy with no gate has
	// nothing to reset and nothing was left over by a gate that does not
	// exist.
	if trailing, err := a.ResetConsumerGate(context.Background(), "test-set-0"); err != nil || trailing != 0 {
		t.Fatalf("resetting a proxy with no gate must be silent, got trailing=%d err=%v", trailing, err)
	}
}

// An arm the proxy refuses reaches the caller as an error, which
// SimulateRequest turns into a refusal result carrying
// CONSUMER_TRIGGER_NOT_DELIVERED rather than a verdict about the worker.
func TestAnArmFailureIsPropagated(t *testing.T) {
	want := errors.New("no recorded trigger is resident")
	a := agentWith(&gateProxy{armErr: want})
	if err := a.ArmConsumerTrigger(context.Background(), models.ConsumerArm{TestID: "test-1"}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

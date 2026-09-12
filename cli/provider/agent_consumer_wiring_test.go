package provider

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// THE ONE LINE THAT ACTIVATES THE RECORD HALF.
//
// GetAgent is the only place that holds both the proxy (which mints Kind:
// Consumer test cases from role-tagged mocks) and the ingress manager (which
// owns the channel every test case reaches persistence through), so it is
// where they are joined. Proxy.startConsumerRecorder deliberately leaves the
// recorder NIL when no test-case sink has been installed — a recorder that
// resolved windows for tests that can never reach disk would corrupt the mock
// mapping — so dropping `p.SetConsumerTestCases(ip.TCChan())` silently
// disables the whole record half.
//
// It was measured: deleting that line left `go test ./cli/... ./pkg/agent/...`
// green. The proxy-side test covers Proxy in isolation with a hand-installed
// channel; nothing covered the provider that has to install it, which is
// exactly the line a bad merge, a GetAgent refactor or an embedder wiring its
// own provider would drop.
func TestGetAgentJoinsTheConsumerRecorderToTheIngressChannel(t *testing.T) {
	cfg := &config.Config{}
	logger := zap.NewNop()

	svc, err := GetAgent(context.Background(), "agent", cfg, logger)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	a, ok := svc.(*agent.Agent)
	if !ok {
		t.Fatalf("GetAgent returned %T, want *agent.Agent", svc)
	}
	p, ok := a.Proxy.(*proxy.Proxy)
	if !ok {
		t.Fatalf("the agent's proxy is %T, want *proxy.Proxy", a.Proxy)
	}

	if p.ConsumerRecorder() != nil {
		t.Fatal("a proxy that has not started a recording session must have no consumer recorder")
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if p.ConsumerRecorder() == nil {
		t.Fatal("the proxy has no consumer recorder after starting a recording session: " +
			"GetAgent did not install the ingress manager's test-case channel, so every consumer " +
			"unit the parsers observe is minted into nowhere and the record half of consumer " +
			"support is silently inert")
	}
}

// The delivery gate is the replay half of the same seam: the parsers find it
// on their context and the agent instrumentation arms it, so both have to be
// the SAME instance. A proxy built by New always has one.
func TestGetAgentExposesTheConsumerDeliveryGate(t *testing.T) {
	svc, err := GetAgent(context.Background(), "agent", &config.Config{}, zap.NewNop())
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	p, ok := svc.(*agent.Agent).Proxy.(*proxy.Proxy)
	if !ok {
		t.Fatal("the agent's proxy is not a *proxy.Proxy")
	}
	if p.ConsumerGate() == nil {
		t.Fatal("the proxy has no consumer delivery gate; nothing could ever be armed and every " +
			"consumer test would be refused CONSUMER_UNSUPPORTED_AGENT")
	}
}

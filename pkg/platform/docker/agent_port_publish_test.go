package docker

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestGenerateKeployAgentService_PublishesAgentPortLoopbackOnly guards the
// fix for the unauthenticated-agent-exposure report: the agent control-plane
// HTTP server carries no authentication (it streams live TLS session keys on
// /agent/pcap/keylog and accepts unauthenticated /agent/stop and
// /agent/storemocks), so its published port must reach only the host's own
// loopback, never every host-network interface.
func TestGenerateKeployAgentService_PublishesAgentPortLoopbackOnly(t *testing.T) {
	t.Parallel()

	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{},
	}).GenerateKeployAgentService(models.SetupOptions{
		KeployContainer: "keploy-agent",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_TEST,
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}

	ports := mappingValue(serviceNode, "ports")
	if ports == nil {
		t.Fatalf("expected ports block")
	}
	wantAgentPublish := "127.0.0.1:16789:16789"
	if !sequenceContains(ports, wantAgentPublish) {
		t.Fatalf("expected agent port published loopback-only as %q, got %s", wantAgentPublish, formatSequence(ports))
	}

	// The proxy port has no auth concern (it's the traffic-interception
	// listener app containers are meant to reach) and must stay published on
	// every interface.
	wantProxyPublish := "16790:16790"
	if !sequenceContains(ports, wantProxyPublish) {
		t.Fatalf("expected proxy port published on all interfaces as %q, got %s", wantProxyPublish, formatSequence(ports))
	}
}

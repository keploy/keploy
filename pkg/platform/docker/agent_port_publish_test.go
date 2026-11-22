package docker

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestGenerateKeployAgentService_PublishesAgentPortLoopbackOnly guards the fix
// for the agent-exposure report: the agent control-plane HTTP server streams
// live TLS session keys on /agent/pcap/keylog and accepts session-mutating
// POSTs on /agent/stop and /agent/storemocks. Requests now carry a bearer
// token (routes.Authenticate), and its published port must still reach only
// the host's own loopback rather than every host-network interface.
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
	// Presence alone is not the invariant. A ports block carrying BOTH the
	// loopback-scoped mapping and a bare one would satisfy the check above
	// while leaving the control plane on every host interface, which is the
	// exposure this test exists to prevent.
	if unrestricted := "16789:16789"; sequenceContains(ports, unrestricted) {
		t.Fatalf("agent port is also published unrestricted as %q, which re-exposes the unauthenticated control plane on every host interface; got %s", unrestricted, formatSequence(ports))
	}

	// The proxy's and the DNS server's ports are reached through the shared
	// namespace (`network_mode: service:keploy-agent`) and are not published
	// at all. The agent binds them there and moves off one another socket
	// holds; a publish claimed the proxy's on the host too, where two runs
	// started together on the configured ports collided.
	for _, p := range []string{"16790:16790", "127.0.0.1:16790:16790", "16791:16791", "127.0.0.1:16791:16791"} {
		if sequenceContains(ports, p) {
			t.Fatalf("the proxy's or the DNS server's port is published as %q; the app reaches them through the agent's network namespace, got %s", p, formatSequence(ports))
		}
	}
}

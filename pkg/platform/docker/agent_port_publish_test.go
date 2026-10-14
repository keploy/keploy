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

	// The proxy port is scoped the same way, and for a reason of its own. It
	// is the interception point for the application's outgoing dependency
	// calls, so reaching it means being able to drive mock matching and to see
	// what a recorded dependency answers.
	//
	// The application does not need the publish to get there: it runs in the
	// agent's own network namespace (`network_mode: service:keploy-agent`) and
	// reaches the proxy over that namespace's loopback. The DNS port, used the
	// same way by the same container, is not published at all — which is the
	// clearest evidence that this publish was never what made interception
	// work.
	wantProxyPublish := "127.0.0.1:16790:16790"
	if !sequenceContains(ports, wantProxyPublish) {
		t.Fatalf("expected proxy port published loopback-only as %q, got %s", wantProxyPublish, formatSequence(ports))
	}
	if unrestricted := "16790:16790"; sequenceContains(ports, unrestricted) {
		t.Fatalf("proxy port is also published unrestricted as %q, exposing the interception listener on every host interface; got %s", unrestricted, formatSequence(ports))
	}

	// The DNS port is reached through the shared namespace and must not be
	// published at all. If it ever is, the reasoning above stops holding.
	for _, dns := range []string{"16791:16791", "127.0.0.1:16791:16791"} {
		if sequenceContains(ports, dns) {
			t.Fatalf("DNS port is published as %q; the app reaches it through the agent's network namespace and should need no publish, got %s", dns, formatSequence(ports))
		}
	}
}

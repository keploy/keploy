package app

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// In mock mode the app container learns where the agent is, so a test can mark its own start and end.
func TestDockerRunGetsTheAgentAddressInMockMode(t *testing.T) {
	a := NewApp(zap.NewNop(), "docker run --rm orders-e2e", nil, models.SetupOptions{MockMode: true, AgentPort: 59226})
	a.keployContainer = "keploy-v3-1"
	if err := a.modifyDockerRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.cmd, "-e KEPLOY_MOCK_AGENT=http://localhost:59226 ") {
		t.Fatalf("mock mode must pass the agent address: %s", a.cmd)
	}
	if !strings.Contains(a.cmd, "-e KEPLOY_MOCK_AGENT_TOKEN ") || strings.Contains(a.cmd, "KEPLOY_MOCK_AGENT_TOKEN=") {
		t.Fatalf("mock mode must pass the agent token by name only: %s", a.cmd)
	}
	b := NewApp(zap.NewNop(), "docker run --rm orders-e2e", nil, models.SetupOptions{AgentPort: 59226})
	b.keployContainer = "keploy-v3-1"
	if err := b.modifyDockerRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.cmd, "KEPLOY_MOCK_AGENT") {
		t.Fatalf("classic record must not: %s", b.cmd)
	}
}

// Each docker-run run's application mounts its own run's CA volume, the one
// its agent writes to. With one name for every run, a second run's agent
// overwrote the first's CA while the first ran, and the first's application
// refused its own agent's certificates.
func TestDockerRunMountsItsOwnRunsCAVolume(t *testing.T) {
	for agent, want := range map[string]string{
		"keploy-v3-0a1b2c3d4e5f6071": "-v keploy-tls-certs-0a1b2c3d4e5f6071:/tmp/keploy-tls:ro ",
		"keploy-v3-8899aabbccddeeff": "-v keploy-tls-certs-8899aabbccddeeff:/tmp/keploy-tls:ro ",
	} {
		a := NewApp(zap.NewNop(), "docker run --rm orders-e2e", nil, models.SetupOptions{})
		a.keployContainer = agent
		if err := a.modifyDockerRun(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(a.cmd, want) || strings.Count(a.cmd, ":/tmp/keploy-tls") != 1 {
			t.Errorf("agent %s: want the one CA mount %q in: %s", agent, want, a.cmd)
		}
	}
}

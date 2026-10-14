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

package app

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// An embedder's run volume (docker.RunVolume) is the run's own in the
// application's command too: added beside the CA's, or, in a command that
// mounts it by the name every run shared, renamed to the run's.
func TestDockerRunMountsItsOwnRunsEmbedderVolume(t *testing.T) {
	prev := docker.DockerConfig.RunVolumes
	docker.DockerConfig.RunVolumes = []docker.RunVolume{{Base: "keploy-sockets-vol", Target: "/tmp"}}
	t.Cleanup(func() { docker.DockerConfig.RunVolumes = prev })
	for cmd, warns := range map[string]int{"docker run --rm orders-e2e": 0, "docker run --rm -v keploy-sockets-vol:/tmp orders-e2e": 1} {
		core, logs := observer.New(zap.WarnLevel)
		a := NewApp(zap.New(core), cmd, nil, models.SetupOptions{})
		a.keployContainer = "keploy-v3-0a1b2c3d4e5f6071"
		if err := a.modifyDockerRun(t.Context()); err != nil {
			t.Fatal(err)
		}
		if strings.Count(a.cmd, "-v keploy-sockets-vol-0a1b2c3d4e5f6071:/tmp ") != 1 || strings.Count(a.cmd, ":/tmp ") != 1 {
			t.Errorf("%q: want the one mount of the run's own volume at /tmp in: %s", cmd, a.cmd)
		}
		if n := logs.FilterMessageSnippet("the command mounts the volume keploy-sockets-vol,").Len(); n != warns {
			t.Errorf("%q: %d WARNs naming the shared name, want %d", cmd, n, warns)
		}
	}
}

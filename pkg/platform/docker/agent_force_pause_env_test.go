package docker

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The debug knob that forces a memory-guard pause by request count is read by
// the agent, so the agent's container is given it when the operator set it,
// under the name the agent reads, and not otherwise.
func TestGenerateKeployAgentService_HandsTheForcedPauseKnobToTheAgent(t *testing.T) {
	if agentForcePauseEnv != memoryguard.ForcePauseEnv {
		t.Fatalf("the agent container is given %q, but the agent reads %q", agentForcePauseEnv, memoryguard.ForcePauseEnv)
	}
	gen := func(t *testing.T) []string {
		t.Helper()
		serviceNode, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			KeployContainer: "keploy-agent",
			AgentPort:       16789,
			ProxyPort:       16790,
			DnsPort:         16791,
			Mode:            models.MODE_RECORD,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		env := mappingValue(serviceNode, "environment")
		if env == nil {
			t.Fatal("expected an environment block")
		}
		var out []string
		for _, n := range env.Content {
			out = append(out, n.Value)
		}
		return out
	}

	t.Setenv(memoryguard.ForcePauseEnv, "100:120")
	env := gen(t)
	found := false
	for _, e := range env {
		found = found || e == memoryguard.ForcePauseEnv+"=100:120"
	}
	if !found {
		t.Fatalf("the agent's environment %q does not carry %s=100:120", env, memoryguard.ForcePauseEnv)
	}

	t.Setenv(memoryguard.ForcePauseEnv, "")
	for _, e := range gen(t) {
		if strings.HasPrefix(e, memoryguard.ForcePauseEnv+"=") {
			t.Fatalf("the agent's environment carries %q, but the operator did not set the knob", e)
		}
	}
}

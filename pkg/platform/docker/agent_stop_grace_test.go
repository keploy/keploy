package docker

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Under `keploy record` the agent's service gives compose a stop grace far
// longer than any drain: compose stops the agent with the project, also when
// the app exits on its own, and the agent then waits for the recording to save
// what it captured, and goes once it has. Compose's default 10s killed it
// mid-drain. Other commands, whose agent holds nothing at a stop, keep
// compose's grace.
func TestGenerateKeployAgentService_StopGracePeriodForARecording(t *testing.T) {
	grace := func(t *testing.T, mode models.Mode, mock bool) string {
		t.Helper()
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			KeployContainer: "keploy-agent", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: mode, MockMode: mock,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		if v := mappingValue(svc, "stop_grace_period"); v != nil {
			return v.Value
		}
		return ""
	}
	if got := grace(t, models.MODE_RECORD, false); got != agentStopGracePeriod {
		t.Errorf("keploy record: stop_grace_period %q, want %q", got, agentStopGracePeriod)
	}
	for _, c := range []struct {
		name string
		mode models.Mode
		mock bool
	}{{"keploy test", models.MODE_TEST, false}, {"keploy mock record", models.MODE_RECORD, true}} {
		if got := grace(t, c.mode, c.mock); got != "" {
			t.Errorf("%s: stop_grace_period %q, want compose's own", c.name, got)
		}
	}
}

package docker

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func TestAgentHealthcheckStartPeriod(t *testing.T) {
	if got := agentHealthcheckStartPeriod(); got.Seconds() != 600 {
		t.Errorf("default = %v, want 600s", got)
	}
	t.Setenv("KEPLOY_AGENT_HEALTHCHECK_START_PERIOD_SECONDS", "1800")
	if got := agentHealthcheckStartPeriod(); got.Seconds() != 1800 {
		t.Errorf("override 1800 = %v, want 1800s", got)
	}
	for _, bad := range []string{"0", "-5", "abc", ""} {
		t.Setenv("KEPLOY_AGENT_HEALTHCHECK_START_PERIOD_SECONDS", bad)
		if got := agentHealthcheckStartPeriod(); got.Seconds() != 600 {
			t.Errorf("invalid %q = %v, want default 600s", bad, got)
		}
	}
}

// The generated keploy-agent healthcheck must carry the generous, env-tunable
// start_period so a large mock load (ready file written only after StoreMocks)
// is not marked unhealthy mid-load. Regression guard for the go-memory-load flake.
func TestGenerateKeployAgentService_HealthcheckStartPeriod(t *testing.T) {
	startPeriod := func(t *testing.T) string {
		t.Helper()
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			KeployContainer: "keploy-agent",
			AgentPort:       16789,
			ProxyPort:       16790,
			DnsPort:         16791,
			Mode:            models.MODE_TEST,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		hc := mappingValue(svc, "healthcheck")
		if hc == nil {
			t.Fatal("no healthcheck block")
		}
		sp := mappingValue(hc, "start_period")
		if sp == nil || sp.Kind != yaml.ScalarNode {
			t.Fatalf("no scalar start_period in healthcheck")
		}
		return sp.Value
	}

	if got := startPeriod(t); got != "600s" {
		t.Errorf("default healthcheck start_period = %q, want 600s (covers a large mock load)", got)
	}

	// interval/timeout/retries must be untouched — guards against the start_period
	// change accidentally cross-wiring to a neighbouring healthcheck field.
	for _, tc := range []struct{ key, want string }{{"interval", "5s"}, {"timeout", "5s"}, {"retries", "60"}} {
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			KeployContainer: "keploy-agent", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		v := mappingValue(mappingValue(svc, "healthcheck"), tc.key)
		if v == nil || v.Value != tc.want {
			t.Errorf("healthcheck %s = %v, want %q", tc.key, v, tc.want)
		}
	}
	t.Setenv("KEPLOY_AGENT_HEALTHCHECK_START_PERIOD_SECONDS", "900")
	if got := startPeriod(t); got != "900s" {
		t.Errorf("env-tuned start_period = %q, want 900s", got)
	}
}

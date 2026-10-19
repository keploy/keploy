package docker

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// The agent container is given only what keploy names, and the agent's log
// lines are the ones agent-debug.log keeps: KEPLOY_LOG_CONTENT, set for the
// CLI, must reach it, and with it unset the alias must not change at all.
func TestGetAlias_CarriesTheLogContentSwitch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242, Mode: models.MODE_TEST}
	env := " -e " + logcontent.EnvVar + "=1"

	restore := logcontent.Set(false)
	t.Cleanup(restore)
	off, err := getAlias(context.Background(), zap.NewNop(), opts, false)
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}
	if strings.Contains(off, logcontent.EnvVar) {
		t.Fatalf("switch off, yet the agent alias names %s:\n%s", logcontent.EnvVar, off)
	}

	logcontent.Set(true)
	on, err := getAlias(context.Background(), zap.NewNop(), opts, false)
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}
	if !strings.Contains(on, env) {
		t.Fatalf("%s=1, yet the agent alias lacks %q:\n%s", logcontent.EnvVar, env, on)
	}
	if strings.Replace(on, env, "", 1) != off {
		t.Fatalf("the switch changed more of the alias than its variable:\noff: %s\non:  %s", off, on)
	}
}

// The compose agent service is the other launcher.
func TestGenerateKeployAgentService_CarriesTheLogContentSwitch(t *testing.T) {
	environment := func(t *testing.T) []string {
		t.Helper()
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		var out []string
		if env := mappingValue(svc, "environment"); env != nil {
			for _, n := range env.Content {
				out = append(out, n.Value)
			}
		}
		return out
	}

	restore := logcontent.Set(false)
	t.Cleanup(restore)
	for _, e := range environment(t) {
		if strings.HasPrefix(e, logcontent.EnvVar) {
			t.Fatalf("switch off, yet the compose agent service is given %s", e)
		}
	}

	logcontent.Set(true)
	for _, e := range environment(t) {
		if e == logcontent.EnvVar+"=1" {
			return
		}
	}
	t.Fatalf("%s=1, yet the compose agent service is not given it: %v", logcontent.EnvVar, environment(t))
}

package http

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.uber.org/zap"
)

// sudo drops KEPLOY_INTERNAL and the CI's variables on the way to a native
// agent, whose telemetry then counted as a user's: a run that is keploy's own
// passes the mark as the agent's flag, and a user's run's argv has none.
func TestNativeAgentArgs_CarryKeploysOwnMark(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	t.Setenv("DOCKER_HOST", "unix:///run/test-docker.sock")
	opts := models.SetupOptions{AgentPort: 26798, ProxyPort: 26799, DnsPort: 26800, AgentURI: "http://localhost:26798/agent", Mode: models.MODE_RECORD}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "a user's run"},
		{name: "KEPLOY_INTERNAL=1", env: map[string]string{telemetry.InternalEnv: "1"}, want: true},
		{name: "keploy's CI", env: map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "keploy/keploy"}, want: true},
		{name: "a customer's CI", env: map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/shop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range telemetry.InternalVariables() {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			args := New(zap.NewNop(), nil, &config.Config{}).nativeAgentArgs(opts)
			if got := slices.Contains(args, "--"+telemetry.AgentFlag); got != tc.want {
				t.Fatalf("native agent argv has --%s: %v, want %v: %q", telemetry.AgentFlag, got, tc.want, args)
			}
		})
	}
}

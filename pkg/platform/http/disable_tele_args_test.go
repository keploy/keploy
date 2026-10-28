package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// `keploy record --disable-tele` still resolved and contacted
// telemetry.keploy.io from the native agent: a keploy process of its own,
// which pings telemetry at its start from its own config, and its argv never
// carried the CLI's --disable-tele. The argv carries the CLI's value, exactly
// once, whatever it is.
func TestNativeAgentArgs_CarryTheCLIsDisableTele(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	// With DOCKER_HOST set the launcher does not ask the host's docker CLI
	// for its context (containerkey.AgentArgs).
	t.Setenv("DOCKER_HOST", "unix:///run/test-docker.sock")
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_RECORD}
	for _, disabled := range []bool{true, false} {
		a := New(zap.NewNop(), nil, &config.Config{DisableTele: disabled})
		args := a.nativeAgentArgs(opts)
		want := "--disable-tele=false"
		if disabled {
			want = "--disable-tele=true"
		}
		var got []string
		for _, arg := range args {
			if arg == "--disable-tele" || strings.HasPrefix(arg, "--disable-tele=") {
				got = append(got, arg)
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("CLI DisableTele=%t: the native agent argv carries %q, want one %q: %q", disabled, got, want, args)
		}
	}
}

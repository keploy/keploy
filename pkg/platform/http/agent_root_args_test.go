package http

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// `keploy record --disable-tele` still resolved and contacted
// telemetry.keploy.io from the native agent: a keploy process of its own,
// which resolves the root settings again from the same keploy.yml the CLI's
// flags may have overridden, and its argv never carried the CLI's
// --disable-tele. The argv carries the CLI's value of each root setting
// (agentargs.Root), true or false, exactly once.
func TestNativeAgentArgs_CarryTheCLIsRootSettings(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	// With DOCKER_HOST set the launcher does not ask the host's docker CLI
	// for its context (containerkey.AgentArgs).
	t.Setenv("DOCKER_HOST", "unix:///run/test-docker.sock")
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_RECORD}
	settings := []struct {
		flag string
		set  func(*config.Config)
	}{
		{"", func(*config.Config) {}},
		{"--debug", func(c *config.Config) { c.Debug = true }},
		{"--disable-tele", func(c *config.Config) { c.DisableTele = true }},
		{"--disable-ansi", func(c *config.Config) { c.DisableANSI = true }},
		{"--disable-mapping", func(c *config.Config) { c.DisableMapping = true }},
	}
	for _, on := range settings {
		conf := &config.Config{}
		on.set(conf)
		args := New(zap.NewNop(), nil, conf).nativeAgentArgs(opts)
		for _, s := range settings[1:] {
			var got []string
			for _, arg := range args {
				if arg == s.flag || strings.HasPrefix(arg, s.flag+"=") {
					got = append(got, arg)
				}
			}
			want := fmt.Sprintf("%s=%t", s.flag, s.flag == on.flag)
			if len(got) != 1 || got[0] != want {
				t.Errorf("CLI with only %q set: the native agent argv carries %q, want one %q: %q", on.flag, got, want, args)
			}
		}
	}
}

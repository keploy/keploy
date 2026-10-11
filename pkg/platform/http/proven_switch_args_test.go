package http

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The native agent is started through sudo, which drops KEPLOY_PROVEN, so the
// switch must reach it as an argv flag; with the switch off the argv must not
// change at all.
func TestNativeAgentArgs_CarryTheProvenSwitch(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	flag := "--" + provenswitch.AgentFlag
	a := New(zap.NewNop(), nil, &config.Config{})
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_TEST}

	t.Setenv(provenswitch.Env, "")
	if args := a.nativeAgentArgs(opts); slices.Contains(args, flag) {
		t.Fatalf("switch off, yet the native agent argv carries %s: %v", flag, args)
	}

	t.Setenv(provenswitch.Env, "1")
	if args := a.nativeAgentArgs(opts); !slices.Contains(args, flag) {
		t.Fatalf("KEPLOY_PROVEN=1, yet the native agent argv lacks %s, so the agent's parsers run the old paths: %v", flag, args)
	}
}

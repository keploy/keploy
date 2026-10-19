package http

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// The native agent writes the lines agent-debug.log keeps, and a user who is
// not root starts it through sudo, which drops KEPLOY_LOG_CONTENT: the opt-in
// must reach it as an argv flag, and with the switch off the argv must not
// change at all.
func TestNativeAgentArgs_CarryTheLogContentSwitch(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	flag := "--" + logcontent.AgentFlag
	a := New(zap.NewNop(), nil, &config.Config{})
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_TEST}

	t.Cleanup(logcontent.Set(false))
	off := a.nativeAgentArgs(opts)
	if slices.Contains(off, flag) {
		t.Fatalf("switch off, yet the native agent argv carries %s: %v", flag, off)
	}

	logcontent.Set(true)
	on := a.nativeAgentArgs(opts)
	if !slices.Contains(on, flag) {
		t.Fatalf("%s=1, yet the native agent argv lacks %s, so an agent started through sudo logs no content: %v", logcontent.EnvVar, flag, on)
	}
	if i := slices.Index(on, flag); !slices.Equal(slices.Delete(slices.Clone(on), i, i+1), off) {
		t.Fatalf("the switch changed more of the argv than its flag:\noff: %v\non:  %v", off, on)
	}
}

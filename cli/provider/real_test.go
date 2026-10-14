package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Under the proven switch keploy mock clears the keploy.yml keys that made
// calls live, with one notice that names them (and, for bypassRules, where
// the app's own port goes); without it they are untouched.
func TestDropPassthroughKeys(t *testing.T) {
	live := func() *config.Config {
		cfg := &config.Config{}
		cfg.Record.GlobalPassthrough = true
		cfg.Agent.OpportunisticTLSIntercept = true
		cfg.Record.PassThroughPorts = []models.PassThroughRule{{Port: 8126}}
		cfg.Record.PassThroughHosts = []models.PassThroughRule{{Host: "telemetry.example.com"}}
		cfg.BypassRules = []models.BypassRule{{Port: 5432}}
		return cfg
	}
	t.Setenv(provenswitch.Env, "")
	cfg := live()
	dropPassthroughKeys(zap.NewNop(), cfg)
	require.Equal(t, live(), cfg)

	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.WarnLevel)
	cfg = live()
	dropPassthroughKeys(zap.New(core), cfg)
	require.False(t, cfg.Record.GlobalPassthrough)
	require.False(t, cfg.Agent.OpportunisticTLSIntercept)
	require.Nil(t, cfg.Record.PassThroughPorts)
	require.Nil(t, cfg.Record.PassThroughHosts)
	require.Nil(t, cfg.BypassRules)
	require.Equal(t, 1, logs.Len())
	e := logs.All()[0]
	require.Contains(t, e.Message, "mock.real")
	require.Equal(t, []any{"globalPassthrough", "opportunisticTlsIntercept", "record.passThroughPorts/passThroughHosts", "bypassRules"}, e.ContextMap()["ignored"])
	require.Contains(t, e.ContextMap()["app_port"], "--pass-through-ports")

	core, logs = observer.New(zap.WarnLevel)
	dropPassthroughKeys(zap.New(core), &config.Config{})
	require.Zero(t, logs.Len(), "nothing to ignore, nothing said")
}

// keploy record and keploy test do not read mock.real yet: under the switch
// a list for them is refused, naming its destinations, rather than recorded
// and mocked; without the switch, or with no list, nothing is.
func TestRefuseRealOutsideMock(t *testing.T) {
	cfg := &config.Config{}
	cfg.Mock.Real = []string{"localhost:5432", "container:redis"}
	t.Setenv(provenswitch.Env, "1")
	for _, name := range []string{"record", "test"} {
		err := refuseRealOutsideMock(cfg, name)
		require.ErrorContains(t, err, "keploy "+name)
		require.ErrorContains(t, err, "localhost:5432, container:redis")
		require.ErrorContains(t, err, "keploy mock")
	}
	require.NoError(t, refuseRealOutsideMock(&config.Config{}, "record"))
	t.Setenv(provenswitch.Env, "")
	require.NoError(t, refuseRealOutsideMock(cfg, "record"))
}

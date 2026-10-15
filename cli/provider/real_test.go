package provider

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func passthroughCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().Bool("global-passthrough", false, "")
	cmd.Flags().Bool("opportunistic-tls-intercept", false, "")
	cmd.Flags().Bool("mocking", true, "")
	cmd.Flags().UintSlice("pass-through-ports", nil, "")
	require.NoError(t, cmd.Flags().Parse(args))
	return cmd
}

// Under the proven switch a flag that made calls live is refused, naming
// mock.real; without it, or not given, nothing is.
func TestRefusePassthroughFlags(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, arg := range []string{"--global-passthrough", "--opportunistic-tls-intercept", "--mocking=false", "--pass-through-ports=8126"} {
		err := refusePassthroughFlags(passthroughCmd(t, arg), passthroughFlags...)
		require.ErrorContains(t, err, "mock.real", arg)
		require.ErrorContains(t, err, "U18", arg)
	}
	require.NoError(t, refusePassthroughFlags(passthroughCmd(t), passthroughFlags...))
	t.Setenv(provenswitch.Env, "")
	require.NoError(t, refusePassthroughFlags(passthroughCmd(t, "--global-passthrough"), passthroughFlags...))
}

// Under the proven switch the keploy.yml keys that made calls live are
// cleared, with one notice that names them (and, for keploy mock's
// bypassRules, where the app's own port goes); without it they are
// untouched.
func TestDropPassthroughKeys(t *testing.T) {
	live := func() *config.Config {
		cfg := &config.Config{}
		cfg.Record.GlobalPassthrough = true
		cfg.Agent.OpportunisticTLSIntercept = true
		cfg.Test.Mocking = false
		cfg.Record.PassThroughPorts = []models.PassThroughRule{{Port: 8126}}
		cfg.Record.PassThroughHosts = []models.PassThroughRule{{Host: "telemetry.example.com"}}
		cfg.BypassRules = []models.BypassRule{{Port: 5432}}
		return cfg
	}
	t.Setenv(provenswitch.Env, "")
	cfg := live()
	dropPassthroughKeys(zap.NewNop(), cfg, "test")
	require.Equal(t, live(), cfg)

	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.WarnLevel)
	cfg = live()
	dropPassthroughKeys(zap.New(core), cfg, "test")
	require.False(t, cfg.Record.GlobalPassthrough)
	require.False(t, cfg.Agent.OpportunisticTLSIntercept)
	require.True(t, cfg.Test.Mocking)
	require.Nil(t, cfg.Record.PassThroughPorts)
	require.Nil(t, cfg.Record.PassThroughHosts)
	require.Nil(t, cfg.BypassRules)
	require.Equal(t, 1, logs.Len())
	e := logs.All()[0]
	require.Contains(t, e.Message, "mock.real")
	require.Equal(t, []any{"globalPassthrough", "opportunisticTlsIntercept", "test.mocking: false", "record.passThroughPorts/passThroughHosts", "bypassRules"}, e.ContextMap()["ignored"])
	require.NotContains(t, e.ContextMap(), "app_port")

	core, logs = observer.New(zap.WarnLevel)
	cfg = live()
	dropPassthroughKeys(zap.New(core), cfg, "mock")
	require.False(t, cfg.Test.Mocking, "keploy mock has no test.mocking to drop")
	require.Contains(t, logs.All()[0].ContextMap()["app_port"], "--pass-through-ports")

	core, logs = observer.New(zap.WarnLevel)
	dropPassthroughKeys(zap.New(core), &config.Config{Test: config.Test{Mocking: true}}, "test")
	require.Zero(t, logs.Len(), "nothing to ignore, nothing said")
}

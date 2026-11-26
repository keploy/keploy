package provider

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.uber.org/zap"
)

// A native agent gets KEPLOY_INTERNAL as its --keploy-internal flag (sudo
// drops the variable): the agent marks its own run from it, so its telemetry
// is keploy's own, and without the flag it marks nothing.
func TestAgentInternalFlagMarksTheAgentsRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "with the flag", args: []string{"--" + telemetry.AgentFlag}, want: "1"},
		{name: "without it", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(telemetry.InternalEnv, "")
			runAgentFlags(t, tc.args...)
			if got := os.Getenv(telemetry.InternalEnv); got != tc.want {
				t.Fatalf("`keploy agent %v`: %s=%q, want %q", tc.args, telemetry.InternalEnv, got, tc.want)
			}
		})
	}
}

// The agent subcommand accepts the hidden --installation-id flag forwarded by
// native launchers so sudo's environment/HOME reset never causes the agent to
// mint a second installation ID.
func TestAgentAcceptsHiddenInstallationIDFlag(t *testing.T) {
	cfg := config.New()
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	agent := &cobra.Command{Use: "agent"}
	if err := c.AddFlags(agent); err != nil {
		t.Fatalf("AddFlags(agent): %v", err)
	}
	f := agent.Flags().Lookup(telemetry.AgentInstallationIDFlag)
	if f == nil {
		t.Fatalf("agent command missing --%s flag", telemetry.AgentInstallationIDFlag)
	}
	if !f.Hidden {
		t.Errorf("--%s flag must be hidden", telemetry.AgentInstallationIDFlag)
	}
	const want = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := agent.ParseFlags([]string{"--" + telemetry.AgentInstallationIDFlag + "=" + want}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := agent.Flags().GetString(telemetry.AgentInstallationIDFlag)
	if err != nil || got != want {
		t.Fatalf("GetString(%s) = %q, %v; want %q, nil", telemetry.AgentInstallationIDFlag, got, err, want)
	}
}

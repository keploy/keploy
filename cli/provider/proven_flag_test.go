package provider

import (
	"context"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// runAgentFlags parses an agent argv the way `keploy agent` does and reports
// whether --proven turned the switch on. The seam is swapped for the test, so
// the real switch stays off for the rest of this test binary.
func runAgentFlags(t *testing.T, args ...string) bool {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	enabled := false
	prev := enableProvenSwitch
	enableProvenSwitch = func() { enabled = true }
	t.Cleanup(func() { enableProvenSwitch = prev })

	cfg := config.New()
	cmd, logger := newAgentCmdForTest(t, cfg)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("the agent refused %q: %v", args, err)
	}
	c := NewCmdConfigurator(logger, cfg)
	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}
	if err := c.ValidateFlags(context.Background(), cmd); err != nil {
		t.Fatalf("ValidateFlags: %v", err)
	}
	return enabled
}

// The agent must accept the flag every launcher now appends when the switch is
// on (provenswitch.AgentArgs), and turn the switch on from it: the agent never
// sees KEPLOY_PROVEN itself.
func TestAgentProvenFlagTurnsTheSwitchOn(t *testing.T) {
	if !runAgentFlags(t, "--"+provenswitch.AgentFlag) {
		t.Fatal("`keploy agent --proven` did not turn the proven switch on: the agent would run the old paths while the CLI runs the new ones")
	}
}

// Without the flag the agent leaves the switch alone: today's agent argv must
// mean today's behaviour.
func TestAgentWithoutProvenFlagLeavesTheSwitchOff(t *testing.T) {
	if runAgentFlags(t) {
		t.Fatal("`keploy agent` without --proven turned the proven switch on")
	}
}

// The flag is internal: set by the launcher, never shown to users.
func TestAgentProvenFlagIsHidden(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd, _ := newAgentCmdForTest(t, config.New())
	f := cmd.Flags().Lookup(provenswitch.AgentFlag)
	if f == nil {
		t.Fatalf("`keploy agent` has no --%s flag; an agent started by a CLI with KEPLOY_PROVEN=1 would refuse to start", provenswitch.AgentFlag)
	}
	if !f.Hidden {
		t.Fatalf("--%s is shown in `keploy agent --help`; it is the launcher's, not the user's", provenswitch.AgentFlag)
	}
}

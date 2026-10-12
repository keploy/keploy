package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// runAgentFlags parses an agent argv the way `keploy agent` does and returns
// what the agent pinned its switch to, and whether it pinned at all. The seam is
// swapped for the test (after newAgentCmdForTest, which swaps in a no-op), so
// the real switch stays unpinned for the rest of this test binary.
func runAgentFlags(t *testing.T, args ...string) (on, called bool) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	cfg := config.New()
	cmd, logger := newAgentCmdForTest(t, cfg)
	prev := pinProvenSwitch
	pinProvenSwitch = func(v bool) { on, called = v, true }
	t.Cleanup(func() { pinProvenSwitch = prev })

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
	return on, called
}

// The agent must accept the flag every launcher now appends when the switch is
// on (provenswitch.AgentArgs), and pin its switch on from it: the agent never
// sees KEPLOY_PROVEN itself.
func TestAgentProvenFlagPinsTheSwitchOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	on, called := runAgentFlags(t, "--"+provenswitch.AgentFlag)
	if !called || !on {
		t.Fatalf("`keploy agent --proven` pinned=%v on=%v: the agent would run the old paths while the CLI runs the new ones", called, on)
	}
}

// Without the flag the agent pins its switch OFF, even with KEPLOY_PROVEN=1 in
// its own environment (an agent some other launcher started, such as
// k8s-proxy's): only the CLI that starts the agent decides.
func TestAgentWithoutProvenFlagPinsTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	on, called := runAgentFlags(t)
	if !called || on {
		t.Fatalf("`keploy agent` without --proven, KEPLOY_PROVEN=1 in its environment: pinned=%v on=%v; want pinned off", called, on)
	}
}

// The agent's own environment does not decide, so an unreadable value there
// does not stop the agent either: its switch comes from the flag.
func TestAgentIgnoresAnUnreadableProvenValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "yes")
	if on, called := runAgentFlags(t, "--"+provenswitch.AgentFlag); !called || !on {
		t.Fatalf("pinned=%v on=%v; the agent's switch comes from its flag", called, on)
	}
}

// The seam must default to the real pin: pointing it at anything else would
// leave every agent following nothing.
func TestPinProvenSwitchDefaultsToProvenswitchPin(t *testing.T) {
	if reflect.ValueOf(pinProvenSwitch).Pointer() != reflect.ValueOf(provenswitch.Pin).Pointer() {
		t.Fatal("pinProvenSwitch is not provenswitch.Pin: the agent's --proven flag decides nothing")
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

// A KEPLOY_PROVEN keploy cannot read is refused before a command runs, rather
// than meaning "off" without a word.
func TestValidateFlagsRefusesAnUnreadableProvenValue(t *testing.T) {
	for _, value := range []string{"yes", "on", "ture"} {
		t.Setenv(provenswitch.Env, value)
		cfg := config.New()
		c, cmd := reportCmd(t, cfg)
		err := c.ValidateFlags(context.Background(), cmd)
		if err == nil || !strings.Contains(err.Error(), provenswitch.Env) {
			t.Fatalf("%s=%q: ValidateFlags = %v; a staging run with a typo would run the old paths", provenswitch.Env, value, err)
		}
	}
	t.Setenv(provenswitch.Env, "1")
	cfg := config.New()
	c, cmd := reportCmd(t, cfg)
	if err := c.ValidateFlags(context.Background(), cmd); err != nil && strings.Contains(err.Error(), provenswitch.Env) {
		t.Fatalf("KEPLOY_PROVEN=1 was refused: %v", err)
	}
}

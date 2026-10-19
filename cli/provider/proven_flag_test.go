package provider

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/log"
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
	t.Setenv(provenswitch.Env, "0")
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

// pinCaseEnv names, in the child process TestAgentSaysItsSwitchIsOnOnlyWhenPinnedOn
// starts for each case, the case that child runs.
const pinCaseEnv = "KEPLOY_TEST_PROVEN_PIN_CASE"

// The agent says its switch is on, at Debug, exactly when it pinned it on: the
// line is the agent's own record, in its log, that it got --proven, which the
// mock_mismatch_streaming lane greps for. Driven through the real pin
// (provenswitch.Pin) and the real logger the agent rebuilds for itself. A pin
// lasts for the rest of its process by design, and nothing may undo it, so
// each case runs in a process of its own: this test starts this test binary
// again for it, and the pin ends with that process.
func TestAgentSaysItsSwitchIsOnOnlyWhenPinnedOn(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{"--proven", "", []string{"--" + provenswitch.AgentFlag}, true},
		{"no flag, KEPLOY_PROVEN=1 in the agent's environment", "1", nil, false},
		{"no flag", "", nil, false},
	}
	if name := os.Getenv(pinCaseEnv); name != "" {
		for _, tc := range cases {
			if tc.name == name {
				agentSaysItsSwitchIsOn(t, tc.args, tc.want)
				return
			}
		}
		t.Fatalf("no case named %q", name)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := exec.Command(os.Args[0], "-test.run=^TestAgentSaysItsSwitchIsOnOnlyWhenPinnedOn$", "-test.count=1", "-test.v")
			// The last value of a variable in Env is the one the child gets.
			child.Env = append(os.Environ(), pinCaseEnv+"="+tc.name, provenswitch.Env+"="+tc.env)
			out, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("the agent's case failed in its own process: %v\n%s", err, out)
			}
			// A child that ran no test at all also exits 0.
			if !strings.Contains(string(out), "--- PASS: TestAgentSaysItsSwitchIsOnOnlyWhenPinnedOn") {
				t.Fatalf("the agent's case did not run in its own process:\n%s", out)
			}
		})
	}
}

// agentSaysItsSwitchIsOn runs the agent's flag handling on args with the real
// pin, and checks that the agent said its switch is on exactly when want, and
// that its switch is on exactly then. It pins this process's switch for good.
func agentSaysItsSwitchIsOn(t *testing.T, args []string, want bool) {
	t.Helper()
	const line = "the proven model is on in this agent"
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Cleanup(log.TestOnlyResetSink)
	cfg := config.New()
	cmd, logger := newAgentCmdForTest(t, cfg)
	pinProvenSwitch = provenswitch.Pin // the real pin; newAgentCmdForTest restores its seam
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("the agent refused %q: %v", args, err)
	}
	c := NewCmdConfigurator(logger, cfg)
	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}
	cfg.Debug = true
	out := captureStdout(t, func() {
		if err := c.ValidateFlags(context.Background(), cmd); err != nil {
			t.Errorf("ValidateFlags: %v", err)
		}
		_ = logger.Sync()
	})
	if got := strings.Contains(out, line); got != want {
		t.Fatalf("the agent said its switch is on: %v, want %v (pinned on: %v)\n%s", got, want, provenswitch.On(), out)
	}
	if provenswitch.On() != want {
		t.Fatalf("the agent's switch is on: %v, want %v", provenswitch.On(), want)
	}
}

// A native agent is told its client's Docker endpoint (sudo drops
// DOCKER_HOST), through a hidden flag, and resolves container keys there.
func TestAgentDockerHostFlagSetsTheEndpoint(t *testing.T) {
	var got []string
	prev := setDockerHost
	setDockerHost = func(h string) { got = append(got, h) }
	t.Cleanup(func() { setDockerHost = prev })

	runAgentFlags(t, "--"+containerkey.AgentFlag+"=unix:///run/user/1000/docker.sock")
	if len(got) != 1 || got[0] != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("--%s set the endpoint %q; want it once", containerkey.AgentFlag, got)
	}
	got = nil
	runAgentFlags(t)
	if len(got) != 0 {
		t.Fatalf("without the flag the endpoint was set to %q", got)
	}

	viper.Reset()
	cmd, _ := newAgentCmdForTest(t, config.New())
	if f := cmd.Flags().Lookup(containerkey.AgentFlag); f == nil || !f.Hidden {
		t.Fatalf("`keploy agent` has no hidden --%s flag: %+v", containerkey.AgentFlag, f)
	}
}

// An agent in keploy's container has no Docker socket, so under the switch it
// names container destinations from the list its keploy CLI pushes.
func TestDockerModeAgentTakesPushedContainers(t *testing.T) {
	calls := 0
	prev := usePushedContainers
	usePushedContainers = func() { calls++ }
	t.Cleanup(func() { usePushedContainers = prev })

	// runAgentFlags does not pin the real switch, so the agent's argv
	// carries --proven as its launcher passes it, and the switch is on in
	// this process's environment too.
	t.Setenv(provenswitch.Env, "1")
	runAgentFlags(t, "--"+provenswitch.AgentFlag, "--is-docker")
	if calls != 1 {
		t.Fatalf("a docker-mode agent took pushed containers %d times, want once", calls)
	}
	runAgentFlags(t, "--"+provenswitch.AgentFlag)
	if calls != 1 {
		t.Fatal("a native agent took pushed containers")
	}
	// Without the switch nothing is touched.
	t.Setenv(provenswitch.Env, "0")
	runAgentFlags(t, "--is-docker")
	if calls != 1 {
		t.Fatal("a docker-mode agent without the switch touched container keys")
	}
}

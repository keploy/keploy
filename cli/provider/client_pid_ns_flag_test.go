package provider

import (
	"context"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
)

// `keploy agent --client-pid-ns X`, as every launch starts the agent, puts X
// in cfg.Agent.ClientPIDNS, which the agent's readers of the CLI's pid
// compare (utils.ClientPIDHere); an agent started without it (by an older
// CLI) has "", and takes the pid by its number alone.
func TestTheAgentReadsTheCLIsPidNamespace(t *testing.T) {
	t.Cleanup(viper.Reset)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--client-pid", "7"}, ""},
		{[]string{"--client-pid", "7", "--client-pid-ns", "4026531836@6f1d8e2a-5b7c-4d3e-9a10-2b3c4d5e6f70"}, "4026531836@6f1d8e2a-5b7c-4d3e-9a10-2b3c4d5e6f70"},
		{[]string{"--client-pid", "7", "--client-pid-ns", "none"}, "none"},
	} {
		viper.Reset()
		cfg := config.New()
		cmd, logger := newAgentCmdForTest(t, cfg)
		if err := cmd.ParseFlags(tc.args); err != nil {
			t.Fatalf("the agent refused %q: %v", tc.args, err)
		}
		c := NewCmdConfigurator(logger, cfg)
		if err := c.PreProcessFlags(cmd); err != nil {
			t.Fatalf("PreProcessFlags: %v", err)
		}
		if err := c.ValidateFlags(context.Background(), cmd); err != nil {
			t.Fatalf("ValidateFlags: %v", err)
		}
		if cfg.Agent.ClientNSPID != 7 || cfg.Agent.ClientPIDNS != tc.want {
			t.Fatalf("`keploy agent %v`: ClientNSPID %d, ClientPIDNS %q, want 7, %q", tc.args, cfg.Agent.ClientNSPID, cfg.Agent.ClientPIDNS, tc.want)
		}
	}
}

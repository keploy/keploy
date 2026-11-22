package provider

import (
	"context"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
)

// `keploy agent --settle-reads`, as a launcher starts the agent of a run that
// learns noise (models.SetupOptions.SettleReads), sets cfg.Agent.SettleReads;
// without it the agent does not settle its reads.
func TestAgentSettleReadsFlagSetsTheAgentsOption(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{{nil, false}, {[]string{"--settle-reads"}, true}} {
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
		if cfg.Agent.SettleReads != tc.want {
			t.Fatalf("`keploy agent %v`: SettleReads %v, want %v", tc.args, cfg.Agent.SettleReads, tc.want)
		}
	}
	viper.Reset()
}

package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func newTestCmdForTest(t *testing.T, cfg *config.Config) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	if err := NewCmdConfigurator(zap.NewNop(), cfg).AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	return cmd
}

// keploy.yml's test.allowEgress and --allow-egress reach the replay in
// keploy's own spelling, and an entry keploy does not act on stops the run
// before anything starts.
func TestKeployTestReadsAllowEgress(t *testing.T) {
	t.Cleanup(viper.Reset)
	t.Setenv(provenswitch.Env, "")
	for _, tc := range []struct {
		name    string
		yml     string
		args    []string
		want    []string
		refused bool
	}{
		{name: "keploy.yml", yml: "test:\n  allowEgress: [\" DNS \"]\n", want: []string{models.AllowEgressDNS}},
		{name: "flag", args: []string{"--allow-egress", "dns"}, want: []string{models.AllowEgressDNS}},
		{name: "unknown entry in keploy.yml", yml: "test:\n  allowEgress: [\"sqs.us-east-1.amazonaws.com:443\"]\n", refused: true},
		{name: "unknown entry on the flag", args: []string{"--allow-egress", "0.0.0.0/0"}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			cfg := config.New()
			cmd := newTestCmdForTest(t, cfg)
			args := append([]string{"--config-path", writeKeployYML(t, tc.yml)}, tc.args...)
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatalf("ParseFlags(%q): %v", args, err)
			}
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			if err := c.PreProcessFlags(cmd); err != nil {
				t.Fatalf("PreProcessFlags: %v", err)
			}
			err := c.ValidateFlags(context.Background(), cmd)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "allowEgress") {
					t.Fatalf("ValidateFlags = %v, want the test.allowEgress refusal", err)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "allowEgress") {
				t.Fatalf("ValidateFlags refused a supported entry: %v", err)
			}
			if !slices.Equal(cfg.Test.AllowEgress, tc.want) {
				t.Fatalf("cfg.Test.AllowEgress = %q, want %q", cfg.Test.AllowEgress, tc.want)
			}
		})
	}
}

// The agent takes --allow-egress the same way, and refuses what keploy test
// would have refused.
func TestTheAgentReadsAllowEgress(t *testing.T) {
	t.Cleanup(viper.Reset)
	t.Setenv(provenswitch.Env, "")
	for _, tc := range []struct {
		args    []string
		want    []string
		refused bool
	}{
		{args: []string{"--client-pid", "7"}},
		{args: []string{"--client-pid", "7", "--allow-egress=dns"}, want: []string{models.AllowEgressDNS}},
		{args: []string{"--client-pid", "7", "--allow-egress=example.com:443"}, refused: true},
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
		err := c.ValidateFlags(context.Background(), cmd)
		if tc.refused {
			if err == nil {
				t.Fatalf("`keploy agent %v` started with an unknown --allow-egress entry", tc.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ValidateFlags(%v): %v", tc.args, err)
		}
		if !slices.Equal(cfg.Agent.AllowEgress, tc.want) {
			t.Fatalf("`keploy agent %v`: AllowEgress %q, want %q", tc.args, cfg.Agent.AllowEgress, tc.want)
		}
	}
}

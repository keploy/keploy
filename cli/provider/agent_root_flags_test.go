package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/agentargs"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// rootSettingsOf is what the agent shares of a config's root-level settings.
type rootSettingsOf struct{ debug, disableTele, disableANSI, disableMapping bool }

func rootSettingsIn(c *config.Config) rootSettingsOf {
	return rootSettingsOf{c.Debug, c.DisableTele, c.DisableANSI, c.DisableMapping}
}

// Every launcher hands the agent the CLI's resolved root-level settings as
// agentargs.Root words (pkg/platform/http nativeAgentArgs, pkg/platform/docker
// getAlias and GenerateKeployAgentService), so that `keploy record
// --disable-tele` keeps the agent, a keploy process of its own that pings
// telemetry at its start, from resolving the telemetry host, and a docker-mode
// --debug reaches it. The agent's config takes those words as given, false as
// well as true, over the keploy.yml the native agent reads too (--config-path)
// saying the opposite.
func TestTheAgentTakesTheForwardedRootSettings(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	t.Cleanup(viper.Reset)
	// The agent's ValidateFlags pins the proven switch for the rest of the
	// process; keep the real one out of this test (newAgentCmdForTest).
	prev := pinProvenSwitch
	pinProvenSwitch = func(bool) {}
	t.Cleanup(func() { pinProvenSwitch = prev })
	settings := []struct {
		yml string
		set func(*config.Config, bool)
	}{
		{"debug", func(c *config.Config, v bool) { c.Debug = v }},
		{"disableTele", func(c *config.Config, v bool) { c.DisableTele = v }},
		{"disableANSI", func(c *config.Config, v bool) { c.DisableANSI = v }},
		{"disableMapping", func(c *config.Config, v bool) { c.DisableMapping = v }},
	}
	for _, s := range settings {
		for _, v := range []bool{true, false} {
			for _, yml := range []string{"", fmt.Sprintf("%s: %t\n", s.yml, !v)} {
				t.Run(fmt.Sprintf("%s=%t, keploy.yml %q", s.yml, v, yml), func(t *testing.T) {
					// --debug and --disable-ansi rebuild the package-level
					// logger and set the ANSI switch; neither may outlive the
					// test.
					t.Cleanup(log.TestOnlyResetSink)
					ansiWas := models.IsAnsiDisabled
					t.Cleanup(func() { models.IsAnsiDisabled = ansiWas })
					viper.Reset()
					dir := t.TempDir()
					t.Chdir(dir)
					if yml != "" {
						if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(yml), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					var cli config.Config
					s.set(&cli, v)

					cfg := config.New()
					c := NewCmdConfigurator(zap.NewNop(), cfg)
					root := &cobra.Command{Use: "keploy"}
					if err := c.AddFlags(root); err != nil {
						t.Fatalf("AddFlags(keploy): %v", err)
					}
					agent := &cobra.Command{Use: "agent"}
					root.AddCommand(agent)
					if err := c.AddFlags(agent); err != nil {
						t.Fatalf("AddFlags(agent): %v", err)
					}
					args := append([]string{"--config-path", dir}, agentargs.Root(&cli)...)
					if err := agent.ParseFlags(args); err != nil {
						t.Fatalf("ParseFlags(%q): %v", args, err)
					}
					if err := c.PreProcessFlags(agent); err != nil {
						t.Fatalf("PreProcessFlags: %v", err)
					}
					if err := c.ValidateFlags(context.Background(), agent); err != nil {
						t.Fatalf("ValidateFlags: %v", err)
					}
					if got, want := rootSettingsIn(cfg), rootSettingsIn(&cli); got != want {
						t.Fatalf("the agent's root settings = %+v, want the CLI's %+v from %q", got, want, args)
					}
				})
			}
		}
	}
}

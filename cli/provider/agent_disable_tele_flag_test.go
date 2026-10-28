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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The launchers hand the agent the CLI's resolved --disable-tele as
// `--disable-tele=<value>` (pkg/platform/http nativeAgentArgs,
// pkg/platform/docker getAlias and GenerateKeployAgentService), so that
// `keploy record --disable-tele` keeps the agent, a keploy process of its own
// that pings telemetry at its start, from resolving the telemetry host. The
// agent's config takes that word as given, over the keploy.yml the native
// agent reads too (--config-path) saying the opposite.
func TestTheAgentTakesTheForwardedDisableTele(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	t.Cleanup(viper.Reset)
	// The agent's ValidateFlags pins the proven switch for the rest of the
	// process; keep the real one out of this test (newAgentCmdForTest).
	prev := pinProvenSwitch
	pinProvenSwitch = func(bool) {}
	t.Cleanup(func() { pinProvenSwitch = prev })
	for _, disabled := range []bool{true, false} {
		for _, yml := range []string{"", fmt.Sprintf("disableTele: %t\n", !disabled)} {
			t.Run(fmt.Sprintf("--disable-tele=%t, keploy.yml %q", disabled, yml), func(t *testing.T) {
				viper.Reset()
				dir := t.TempDir()
				t.Chdir(dir)
				if yml != "" {
					if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(yml), 0o644); err != nil {
						t.Fatal(err)
					}
				}
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
				args := []string{"--config-path", dir, fmt.Sprintf("--disable-tele=%t", disabled)}
				if err := agent.ParseFlags(args); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}
				if err := c.PreProcessFlags(agent); err != nil {
					t.Fatalf("PreProcessFlags: %v", err)
				}
				if err := c.ValidateFlags(context.Background(), agent); err != nil {
					t.Fatalf("ValidateFlags: %v", err)
				}
				if cfg.DisableTele != disabled {
					t.Fatalf("the agent's DisableTele = %t, want %t from the forwarded flag", cfg.DisableTele, disabled)
				}
			})
		}
	}
}

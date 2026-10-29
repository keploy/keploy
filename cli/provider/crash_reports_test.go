package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/getsentry/sentry-go"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A crash report is telemetry and carries the run's whole log, so the
// crash-report client starts only once ValidateFlags has resolved DisableTele
// (the flag over keploy.yml over the default, and --enable-testing), and only
// with telemetry on: in the CLI's commands and in the agent, which is handed
// the CLI's --disable-tele. main arms the DSN and starts nothing.
func TestTheCrashClientStartsOnlyOnceTelemetryResolvesOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	t.Cleanup(viper.Reset)
	prev := pinProvenSwitch
	pinProvenSwitch = func(bool) {}
	t.Cleanup(func() { pinProvenSwitch = prev })
	reset := func() {
		sentry.CurrentHub().BindClient(nil)
		utils.ArmCrashReports("")
	}
	reset()
	t.Cleanup(reset)

	cases := []struct {
		name   string
		yml    string
		args   []string
		wantOn bool
	}{
		{"no flag, no keploy.yml", "", nil, true},
		{"keploy.yml disableTele: true", "disableTele: true\n", nil, false},
		{"keploy.yml disableTele: false", "disableTele: false\n", nil, true},
		{"--disable-tele over keploy.yml false", "disableTele: false\n", []string{"--disable-tele"}, false},
		{"--disable-tele=false over keploy.yml true", "disableTele: true\n", []string{"--disable-tele=false"}, true},
		{"--enable-testing", "", []string{"--enable-testing"}, false},
	}
	for _, name := range []string{"record", "agent"} {
		for _, tc := range cases {
			t.Run(name+": "+tc.name, func(t *testing.T) {
				reset()
				viper.Reset()
				dir := t.TempDir()
				t.Chdir(dir)
				if tc.yml != "" {
					if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(tc.yml), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				// Nothing listens there: starting a client contacts no one.
				utils.ArmCrashReports("http://public@127.0.0.1:9/1")
				cfg := config.New()
				c := NewCmdConfigurator(zap.NewNop(), cfg)
				root := &cobra.Command{Use: "keploy"}
				if err := c.AddFlags(root); err != nil {
					t.Fatalf("AddFlags(keploy): %v", err)
				}
				cmd := &cobra.Command{Use: name}
				root.AddCommand(cmd)
				if err := c.AddFlags(cmd); err != nil {
					t.Fatalf("AddFlags(%s): %v", name, err)
				}
				if err := cmd.ParseFlags(append([]string{"--config-path", dir}, tc.args...)); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}
				if err := c.PreProcessFlags(cmd); err != nil {
					t.Fatalf("PreProcessFlags: %v", err)
				}
				if sentry.CurrentHub().Client() != nil {
					t.Fatal("a crash-report client runs before ValidateFlags resolved telemetry")
				}
				// record refuses later on for want of -c; telemetry is
				// resolved by then.
				if err := c.ValidateFlags(context.Background(), cmd); err != nil && (name == "agent" || !errors.Is(err, errNoCommand)) {
					t.Fatalf("ValidateFlags: %v", err)
				}
				if on := sentry.CurrentHub().Client() != nil; on != tc.wantOn {
					t.Fatalf("crash-report client running = %t, want %t (DisableTele resolved %t)", on, tc.wantOn, cfg.DisableTele)
				}
			})
		}
	}

	// keploy config runs ValidateFlags alone: neither its flags nor
	// keploy.yml reach cfg, whose DisableTele is then the default, not the
	// user's answer. It starts no client, --disable-tele and keploy.yml's
	// disableTele: true included.
	for _, args := range [][]string{{"--disable-tele"}, nil} {
		t.Run(fmt.Sprintf("config %v, keploy.yml disableTele: true", args), func(t *testing.T) {
			reset()
			viper.Reset()
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte("disableTele: true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			utils.ArmCrashReports("http://public@127.0.0.1:9/1")
			c := NewCmdConfigurator(zap.NewNop(), config.New())
			root := &cobra.Command{Use: "keploy"}
			if err := c.AddFlags(root); err != nil {
				t.Fatalf("AddFlags(keploy): %v", err)
			}
			cmd := &cobra.Command{Use: "config"}
			root.AddCommand(cmd)
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags(config): %v", err)
			}
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if err := c.ValidateFlags(context.Background(), cmd); err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if sentry.CurrentHub().Client() != nil {
				t.Fatal("keploy config started a crash-report client from a DisableTele it never read")
			}
		})
	}
}

package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// `keploy record --pass-through-ports 7002` (or --passThroughPorts) stopped
// at startup from v3.6.13 on: the flag was bound to record.passThroughPorts,
// which #4423 made a list of telemetry rules, and the config failed to load
// with "'record.passThroughPorts[0]' expected a map or struct, got "uint"".
// The flag's ports go to bypassRules; keploy.yml's record.passThroughPorts
// rules stay as written, with the flag or without it, in record and test.
func TestPassThroughPortsFlagAndTheTelemetryRulesBothLoad(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)

	const rules = "record:\n  passThroughPorts:\n    - port: 4317\n      mode: skip\n  passThroughHosts:\n    - host: \"*.datadoghq.com\"\n      mode: recordOne\n"
	const bypass = "bypassRules:\n  - port: 7003\n"
	for _, command := range []string{"record", "test"} {
		for _, tc := range []struct {
			name      string
			yml       string
			args      []string
			wantPorts []uint
			wantRules bool
		}{
			{"--pass-through-ports", "", []string{"--pass-through-ports", "7002"}, []uint{7002}, false},
			{"--passThroughPorts", "", []string{"--passThroughPorts", "7002"}, []uint{7002}, false},
			{"--pass-through-ports, two ports", "", []string{"--pass-through-ports", "7002,7004"}, []uint{7002, 7004}, false},
			{"--pass-through-ports with keploy.yml rules", rules, []string{"--pass-through-ports", "7002"}, []uint{7002}, true},
			{"--passThroughPorts with keploy.yml rules", rules, []string{"--passThroughPorts", "7002"}, []uint{7002}, true},
			{"keploy.yml rules alone", rules, nil, nil, true},
			{"keploy.yml rules and bypassRules", rules + bypass, nil, []uint{7003}, true},
			{"keploy.yml rules and bypassRules, and the flag", rules + bypass, []string{"--pass-through-ports", "7002"}, []uint{7003, 7002}, true},
		} {
			t.Run(command+" "+tc.name, func(t *testing.T) {
				viper.Reset()
				dir := t.TempDir()
				t.Chdir(dir)
				if tc.yml != "" {
					if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(tc.yml), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.MkdirAll(filepath.Join(dir, "keploy"), 0o755); err != nil {
					t.Fatal(err)
				}
				cfg := config.New()
				c := NewCmdConfigurator(zap.NewNop(), cfg)
				root := &cobra.Command{Use: "keploy"}
				cmd := &cobra.Command{Use: command}
				root.AddCommand(cmd)
				if err := c.AddFlags(cmd); err != nil {
					t.Fatalf("AddFlags: %v", err)
				}
				args := append([]string{"--config-path", dir, "-c", "python app.py", "-p", dir}, tc.args...)
				if err := cmd.ParseFlags(args); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}

				if err := c.PreProcessFlags(cmd); err != nil {
					t.Fatalf("PreProcessFlags: %v", err)
				}
				if err := c.ValidateFlags(context.Background(), cmd); err != nil {
					t.Fatalf("ValidateFlags: %v", err)
				}

				if got := config.GetByPassPorts(cfg); !slices.Equal(got, tc.wantPorts) {
					t.Errorf("bypassRules ports = %v, want %v", got, tc.wantPorts)
				}
				wantPorts := []models.PassThroughRule(nil)
				wantHosts := []models.PassThroughRule(nil)
				if tc.wantRules {
					wantPorts = []models.PassThroughRule{{Port: 4317, Mode: models.PassThroughSkip}}
					wantHosts = []models.PassThroughRule{{Host: "*.datadoghq.com", Mode: models.PassThroughRecordOne}}
				}
				if !reflect.DeepEqual(cfg.Record.PassThroughPorts, wantPorts) {
					t.Errorf("record.passThroughPorts = %+v, want %+v", cfg.Record.PassThroughPorts, wantPorts)
				}
				if !reflect.DeepEqual(cfg.Record.PassThroughHosts, wantHosts) {
					t.Errorf("record.passThroughHosts = %+v, want %+v", cfg.Record.PassThroughHosts, wantHosts)
				}
			})
		}
	}
}

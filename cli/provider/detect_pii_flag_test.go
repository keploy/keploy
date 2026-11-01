package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// record.detectPII in keploy.yml turns detection on, in the spelling the
// generated config uses and in the one the flag binds, and only an explicit
// --detect-pii overrides it.
func TestDetectPIIFromConfigAndFlag(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)

	for _, tc := range []struct {
		name string
		yml  string
		args []string
		want bool
	}{
		{"default", "", nil, false},
		{"flag", "", []string{"--detect-pii"}, true},
		{"flag alias", "", []string{"--detectPII"}, true},
		{"keploy.yml", "record:\n  detectPII: true\n", nil, true},
		{"keploy.yml, flag's spelling", "record:\n  detectPii: true\n", nil, true},
		{"keploy.yml turned off by the flag", "record:\n  detectPII: true\n", []string{"--detect-pii=false"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			cmd := &cobra.Command{Use: "record"}
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
			if cfg.Record.DetectPII != tc.want {
				t.Fatalf("Record.DetectPII = %v, want %v", cfg.Record.DetectPII, tc.want)
			}
		})
	}
}

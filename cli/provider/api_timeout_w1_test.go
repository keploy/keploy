package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// keploy test knows whether the bound on the app's answer is the user's own
// (config.Test.APITimeoutSet): --api-timeout, or test.apiTimeout in
// keploy.yml. keploy's own default is neither (row W1).
func TestTheAPITimeoutIsTheUsersOwnOnlyWhenTheyGaveIt(t *testing.T) {
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	for _, tc := range []struct {
		name string
		yml  string
		args []string
		want bool
	}{
		{"keploy's default", "", nil, false},
		{"the flag", "", []string{"--api-timeout", "9"}, true},
		{"keploy.yml", "test:\n  apiTimeout: 9\n", nil, true},
		{"keploy.yml without it", "test:\n  delay: 7\n", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			if tc.yml != "" {
				viper.SetConfigType("yml")
				if err := viper.ReadConfig(strings.NewReader(tc.yml)); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.New()
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "test"}
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "keploy"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := cmd.ParseFlags(append([]string{"-c", "python app.py", "-p", dir}, tc.args...)); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			cfg.Command, cfg.Path = "python app.py", dir
			if err := c.ValidateFlags(context.Background(), cmd); err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if cfg.Test.APITimeoutSet != tc.want {
				t.Errorf("APITimeoutSet = %v, want %v", cfg.Test.APITimeoutSet, tc.want)
			}
		})
	}
}

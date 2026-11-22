package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func TestNormalizeTestRunFlagReachesConfig(t *testing.T) {
	for _, tc := range []struct{ name, configRun, flagRun, want string }{
		{"flag", "", "test-run-1", "test-run-1"},
		{"config only", "test-run-2", "", "test-run-2"},
		{"flag overrides config", "test-run-2", "test-run-1", "test-run-1"},
		{"omitted", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			cfg := config.New()
			cmd := &cobra.Command{Use: "normalize"}
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			require.NoError(t, c.AddFlags(cmd))
			dir := t.TempDir()
			if tc.configRun != "" {
				text := "normalize:\n  testReport: " + tc.configRun + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(text), 0644))
			}
			args := []string{"--config-path", dir}
			if tc.flagRun != "" {
				args = append(args, "--test-run", tc.flagRun)
			}
			require.NoError(t, cmd.ParseFlags(args))
			require.NoError(t, c.PreProcessFlags(cmd))
			require.Equal(t, tc.want, cfg.Normalize.TestRun)
		})
	}
}

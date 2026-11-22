package provider

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func TestIgnoreStaticAssetsConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		flag string
		want bool
	}{
		{name: "default off", yaml: "record: {}"},
		{name: "yaml opt in", yaml: "record:\n  ignoreStaticAssets: true\n", want: true},
		{name: "flag opt in", yaml: "record: {}", flag: "true", want: true},
		{name: "explicit false overrides yaml", yaml: "record:\n  ignoreStaticAssets: true\n", flag: "false"},
	} {
		t.Run("record/"+tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			cfg := config.New()
			cmd := newRecordCmdForTest(t, cfg)
			require.NoError(t, cmd.Flags().Set("config-path", writeKeployYML(t, tc.yaml)))
			if tc.flag != "" {
				require.NoError(t, cmd.Flags().Set("ignore-static-assets", tc.flag))
			}
			require.NoError(t, NewCmdConfigurator(zap.NewNop(), cfg).PreProcessFlags(cmd))
			assert.Equal(t, tc.want, cfg.Record.IgnoreStaticAssets)
		})
	}

	for _, tc := range []struct {
		name    string
		fromYml bool
		flag    string
		want    bool
	}{
		{name: "default off", want: false},
		{name: "yaml opt in", fromYml: true, want: true},
		{name: "flag opt in", flag: "true", want: true},
		{name: "explicit false overrides yaml", fromYml: true, flag: "false", want: false},
	} {
		t.Run("mock-record/"+tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			cfg := config.New()
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "record"}
			require.NoError(t, c.addMockFlags(cmd))
			if tc.fromYml {
				viper.Set("record.ignoreStaticAssets", true)
				cfg.Record.IgnoreStaticAssets = true
			}
			if tc.flag != "" {
				require.NoError(t, cmd.Flags().Set("ignore-static-assets", tc.flag))
			}
			require.NoError(t, c.readMockBool(cmd, "ignore-static-assets", "record.ignoreStaticAssets", &cfg.Record.IgnoreStaticAssets))
			assert.Equal(t, tc.want, cfg.Record.IgnoreStaticAssets)
		})
	}
}

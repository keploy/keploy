package provider

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func TestIncludeStaticAssetsConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		flag string
		want bool
	}{
		{name: "default", yaml: "record: {}"},
		{name: "yaml opt out", yaml: "record:\n  includeStaticAssets: true\n", want: true},
		{name: "flag opt out", yaml: "record: {}", flag: "true", want: true},
		{name: "explicit false overrides yaml", yaml: "record:\n  includeStaticAssets: true\n", flag: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			cfg := config.New()
			cmd := newRecordCmdForTest(t, cfg)
			require.NoError(t, cmd.Flags().Set("config-path", writeKeployYML(t, tc.yaml)))
			if tc.flag != "" {
				require.NoError(t, cmd.Flags().Set("include-static-assets", tc.flag))
			}
			require.NoError(t, NewCmdConfigurator(zap.NewNop(), cfg).PreProcessFlags(cmd))
			assert.Equal(t, tc.want, cfg.Record.IncludeStaticAssets)
		})
	}
}

package provider

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// The flag is off by default, keploy.yml can turn it on, and an explicit flag wins either way.
func TestRecordRequestsFlagRespectsKeployYml(t *testing.T) {
	t.Cleanup(viper.Reset)
	for _, tc := range []struct {
		name    string
		fromYml bool
		flag    string
		want    bool
	}{
		{name: "off by default", want: false},
		{name: "flag turns it on", flag: "true", want: true},
		{name: "keploy.yml turns it on", fromYml: true, want: true},
		{name: "an explicit flag overrides keploy.yml", fromYml: true, flag: "false", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			cfg := &config.Config{}
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "record"}
			if err := c.addMockFlags(cmd); err != nil {
				t.Fatal(err)
			}
			if tc.fromYml {
				viper.Set("mock.recordRequests", true)
				cfg.Mock.RecordRequests = true
			}
			if tc.flag != "" {
				if err := cmd.Flags().Set("record-requests", tc.flag); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.readMockBool(cmd, "record-requests", "mock.recordRequests", &cfg.Mock.RecordRequests); err != nil {
				t.Fatal(err)
			}
			if cfg.Mock.RecordRequests != tc.want {
				t.Fatalf("got %v", cfg.Mock.RecordRequests)
			}
		})
	}
}

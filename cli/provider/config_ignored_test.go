package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// Reading keploy.yml keeps its test.ignoredTests as it gave it
// (config.Test.ConfigIgnoredTests), so a test it names is skipped with
// keploy.yml as the reason; with no keploy.yml, it names none.
func TestReadingKeployYmlKeepsItsIgnoreList(t *testing.T) {
	for _, c := range []struct {
		name string
		yml  string
		want map[string][]string
	}{
		{"a test, and a set whole", "test:\n  ignoredTests:\n    test-set-0: [test-2]\n    test-set-1: []\n",
			map[string][]string{"test-set-0": {"test-2"}, "test-set-1": {}}},
		{"no keploy.yml", "", map[string][]string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			viper.Reset()
			savedFound := IsConfigFileFound
			t.Cleanup(func() { IsConfigFileFound = savedFound; viper.Reset() })
			project, configDir := t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(project, "keploy"), 0o755); err != nil {
				t.Fatal(err)
			}
			if c.yml != "" {
				if err := os.WriteFile(filepath.Join(configDir, "keploy.yml"), []byte(c.yml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.New()
			cfg.Test.ConfigIgnoredTests = map[string][]string{"stale": {}}
			p := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "test"}
			if err := p.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			if err := cmd.ParseFlags([]string{"--path", project, "--config-path", configDir}); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if err := p.PreProcessFlags(cmd); err != nil {
				t.Fatalf("PreProcessFlags: %v", err)
			}
			if got := cfg.Test.ConfigIgnoredTests; len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
				t.Fatalf("kept %v, want %v", got, c.want)
			}
		})
	}
}

package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy test -t "test-set-1, test-set-2" (the help text's own example)
// selects both sets, and says the selection is the command line's own: with
// the proven switch off, a name it gives that is not recorded fails the run,
// and one keploy.yml inherited is left out with a warning
// (Replayer.selectTestSets). -t "" names nothing, and is no selection.
func TestTheTestSetsFlagIsTheCommandLinesOwnSelection(t *testing.T) {
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	for _, tc := range []struct {
		name     string
		args     []string
		want     []string
		fromFlag bool
	}{
		{"the help text's example", []string{"-t", "test-set-1, test-set-2"}, []string{"test-set-1", "test-set-2"}, true},
		{"no -t", nil, nil, false},
		{"an empty -t", []string{"-t", ""}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			// Mirror what viper.Unmarshal leaves behind before ValidateFlags.
			cfg.Command, cfg.Path = "python app.py", dir
			if err := c.ValidateFlags(context.Background(), cmd); err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			got := utils.Keys(cfg.Test.SelectedTests)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("selected %q, want %q", got, tc.want)
			}
			if cfg.Test.SelectedFromCommandLine != tc.fromFlag {
				t.Errorf("SelectedFromCommandLine = %v, want %v", cfg.Test.SelectedFromCommandLine, tc.fromFlag)
			}
		})
	}
}

// keploy templatize -t "test-set-1, test-set-2" reaches Templatize.TestSets
// through viper (the flag is bound to templatize.testSets), each name trimmed,
// and says the names are the command line's own; keploy.yml's
// templatize.testSets is trimmed too, and is not.
func TestTheTemplatizeTestSetsFlagIsTheCommandLinesOwnSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yml      string
		args     []string
		want     []string
		fromFlag bool
	}{
		{"the help text's example", "", []string{"-t", "test-set-1, test-set-2"}, []string{"test-set-1", "test-set-2"}, true},
		{"keploy.yml's", "templatize:\n  testSets: [\"test-set-1 \", \" test-set-2\"]\n", nil, []string{"test-set-1", "test-set-2"}, false},
		{"an empty -t", "", []string{"-t", ""}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			savedFound := IsConfigFileFound
			t.Cleanup(func() { IsConfigFileFound = savedFound; viper.Reset() })
			project, configDir := t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(project, "keploy"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.yml != "" {
				if err := os.WriteFile(filepath.Join(configDir, "keploy.yml"), []byte(tc.yml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.New()
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "templatize"}
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			if err := cmd.ParseFlags(append([]string{"--path", project, "--config-path", configDir}, tc.args...)); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if err := c.PreProcessFlags(cmd); err != nil {
				t.Fatalf("PreProcessFlags: %v", err)
			}
			if err := c.ValidateFlags(context.Background(), cmd); err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if !slices.Equal(cfg.Templatize.TestSets, tc.want) {
				t.Errorf("templatizes %q, want %q", cfg.Templatize.TestSets, tc.want)
			}
			if cfg.Templatize.TestSetsFromCommandLine != tc.fromFlag {
				t.Errorf("TestSetsFromCommandLine = %v, want %v", cfg.Templatize.TestSetsFromCommandLine, tc.fromFlag)
			}
		})
	}
}

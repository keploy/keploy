package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// sampleFlagValue is a value of each flag type the command tree uses, so the
// flag can be passed. A type missing here fails the test, which names it.
var sampleFlagValue = map[string]string{
	"bool":           "true",
	"int":            "1",
	"int32":          "1",
	"int64":          "1",
	"uint":           "1",
	"uint16":         "1",
	"uint32":         "1",
	"uint64":         "1",
	"float64":        "1",
	"duration":       "1s",
	"string":         "x",
	"stringSlice":    "x",
	"stringArray":    "x",
	"uintSlice":      "7002",
	"intSlice":       "1",
	"stringToString": "x=y",
	"myEnum":         "go", // models.Language
}

// TestEveryFlagLeavesTheConfigLoadable: a flag a user passes must not stop
// keploy from loading its config. Every flag is also bound to the config key
// of its own name under the command's section (record.<flag>), and when that
// key holds something else, the config fails to load with the flag passed.
// `keploy record --pass-through-ports 7002` did, from v3.6.13: #4423 made
// record.passThroughPorts hold telemetry rules, and the port list could not
// decode into them ("'record.passThroughPorts[0]' expected a map or struct,
// got "uint""). Each flag of each command that loads the config is passed on
// its own, in a fresh tree, against a keploy.yml that sets those rules.
func TestEveryFlagLeavesTheConfigLoadable(t *testing.T) {
	t.Cleanup(viper.Reset)
	dir := t.TempDir()
	yml := "record:\n  passThroughPorts:\n    - port: 4317\n      mode: skip\n"
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	// PreProcessFlags also looks for <cwd's last dir>.keploy.yml in the cwd.
	t.Chdir(dir)

	type flagOf struct{ path, name, typ string }
	var flags []flagOf
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.PreRunE != nil {
			c.InitDefaultHelpFlag()
			// Merges the persistent flags (config-path) into c.Flags().
			_ = c.ParseFlags(nil)
			c.Flags().VisitAll(func(f *pflag.Flag) {
				if f.Name == "help" || f.Name == "config-path" {
					return
				}
				flags = append(flags, flagOf{c.CommandPath(), f.Name, f.Value.Type()})
			})
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRealRoot())
	if len(flags) < 50 {
		t.Fatalf("found only %d flags in the command tree", len(flags))
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i].path+flags[i].name < flags[j].path+flags[j].name })

	for _, f := range flags {
		value, ok := sampleFlagValue[f.typ]
		if !ok {
			t.Errorf("%s --%s: no sample value for flag type %q", f.path, f.name, f.typ)
			continue
		}
		viper.Reset()
		root := newRealRoot()
		cmd, _, err := root.Find(strings.Fields(f.path)[1:])
		if err != nil {
			t.Fatalf("%s: %v", f.path, err)
		}
		if err := cmd.ParseFlags([]string{"--config-path=" + dir, "--" + f.name + "=" + value}); err != nil {
			t.Errorf("%s --%s=%s: %v", f.path, f.name, value, err)
			continue
		}
		core, logs := observer.New(zap.ErrorLevel)
		if err := provider.NewCmdConfigurator(zap.New(core), config.New()).PreProcessFlags(cmd); err != nil {
			var said []string
			for _, e := range logs.All() {
				said = append(said, fmt.Sprintf("%s %v", e.Message, e.ContextMap()))
			}
			t.Errorf("%s --%s=%s: the config does not load: %v: %s", f.path, f.name, value, err, strings.Join(said, "; "))
		}
	}
}

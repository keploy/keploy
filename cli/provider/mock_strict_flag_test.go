package provider

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// AddFlags intercepts every subcommand whose parent is `mock` and returns
// addMockFlags(cmd), so `keploy mock replay` never reaches AddUncommonFlags —
// the only place --schema-noise-strict used to be registered. The flag was
// therefore unavailable on the one command whose default matcher (bodyMatch,
// top-level key presence only) is what makes a drifted request VALUE get served
// the stale recorded response.
//
// Registration alone is not the feature — pkg/service/mock's
// TestReplay_ForwardsSchemaNoiseStrictToTheProxy pins the other half, that the
// resolved value actually reaches the proxy.
func TestMockReplayRegistersSchemaNoiseStrict(t *testing.T) {
	newMockSubcommand := func(verb string) *cobra.Command {
		parent := &cobra.Command{Use: "mock"}
		child := &cobra.Command{Use: verb}
		parent.AddCommand(child)
		return child
	}

	replay := newMockSubcommand("replay")
	c := NewCmdConfigurator(zap.NewNop(), &config.Config{})
	if err := c.AddFlags(replay); err != nil {
		t.Fatalf("AddFlags(mock replay): %v", err)
	}
	if replay.Flags().Lookup("schema-noise-strict") == nil {
		t.Fatalf("`keploy mock replay` does not register --schema-noise-strict; "+
			"registered flags: %v", flagNames(replay))
	}

	// Parsing it must succeed and be observable through Changed(), which is what
	// validateMockFlags keys off so an unset flag can't clobber the keploy.yml
	// value with its compile-time default.
	if err := replay.Flags().Parse([]string{"--schema-noise-strict"}); err != nil {
		t.Fatalf("parse --schema-noise-strict: %v", err)
	}
	if !replay.Flags().Changed("schema-noise-strict") {
		t.Fatal("--schema-noise-strict did not register as Changed after being passed")
	}
	if v, err := replay.Flags().GetBool("schema-noise-strict"); err != nil || !v {
		t.Fatalf("GetBool(schema-noise-strict) = %v, %v; want true, nil", v, err)
	}

	// Recording cannot match anything, so the flag is replay-only by design.
	record := newMockSubcommand("record")
	if err := NewCmdConfigurator(zap.NewNop(), &config.Config{}).AddFlags(record); err != nil {
		t.Fatalf("AddFlags(mock record): %v", err)
	}
	if record.Flags().Lookup("schema-noise-strict") != nil {
		t.Fatal("`keploy mock record` should not register --schema-noise-strict; it is a match-time knob")
	}
}

func flagNames(cmd *cobra.Command) []string {
	var out []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { out = append(out, f.Name) })
	return out
}

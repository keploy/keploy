package config

import (
	"slices"
	"testing"

	"github.com/spf13/pflag"
	"go.keploy.io/server/v3/utils"
)

// The help text's own example, -t "test-set-1, test-set-2", is read as both
// sets (P189): a comma-separated flag keeps the space after each comma, so the
// second name was " test-set-2", which names no test set, and only the first
// ran. Names are trimmed, and a blank one is no name.
func TestSelectedTestSetsAreTheNamesTheFlagGives(t *testing.T) {
	for _, value := range []string{"test-set-1, test-set-2", " test-set-1 ,test-set-2 ", "test-set-1,, test-set-2,"} {
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.StringSliceP("test-sets", "t", nil, "")
		if err := flags.Parse([]string{"-t", value}); err != nil {
			t.Fatal(err)
		}
		names, err := flags.GetStringSlice("test-sets")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"test-set-1", "test-set-2"}

		conf := &Config{}
		SetSelectedTests(conf, names)
		if got := utils.Keys(conf.Test.SelectedTests); !slices.Equal(sorted(got), want) {
			t.Errorf("-t %q selects the test sets %q, want %q", value, got, want)
		}
		SetSelectedTestSets(conf, names)
		if got := utils.Keys(conf.Report.SelectedTestSets); !slices.Equal(sorted(got), want) {
			t.Errorf("-t %q reports the test sets %q, want %q", value, got, want)
		}
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

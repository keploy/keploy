package cli

import (
	"errors"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.keploy.io/server/v3/utils"
)

// TagUsageErrors tags what cobra raises while parsing the command line, and
// nothing a command returns while it runs: a flag error and an argument-count
// error are usage errors; a command reading a flag it never defined gets
// pflag's own error type back, and that is keploy's bug, not the user's.
func TestTagUsageErrors(t *testing.T) {
	build := func() (*cobra.Command, *int) {
		printed := 0
		root := &cobra.Command{Use: "keploy", SilenceErrors: true, SilenceUsage: true}
		// The configurator's flag-error func prints the error and returns it.
		root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { printed++; return err })
		run := &cobra.Command{Use: "run", RunE: func(*cobra.Command, []string) error { return nil }}
		run.Flags().Int("delay", 0, "")
		diff := &cobra.Command{Use: "diff", Args: cobra.MaximumNArgs(2), RunE: func(*cobra.Command, []string) error { return nil }}
		misuse := &cobra.Command{Use: "misuse", RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.Flags().GetString("never-defined")
			return err
		}}
		root.AddCommand(run, diff, misuse)
		TagUsageErrors(root)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		return root, &printed
	}
	for name, tc := range map[string]struct {
		args    []string
		usage   bool
		printed int
	}{
		"unknown flag":       {[]string{"run", "--nope"}, true, 1},
		"invalid flag value": {[]string{"run", "--delay", "abc"}, true, 1},
		"flag missing value": {[]string{"run", "--delay"}, true, 1},
		"too many arguments": {[]string{"diff", "a", "b", "c"}, true, 0},
		"flag API misuse":    {[]string{"misuse"}, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			root, printed := build()
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil {
				t.Fatal("no error")
			}
			if got := utils.IsUsageError(err); got != tc.usage {
				t.Fatalf("IsUsageError(%v) = %v, want %v", err, got, tc.usage)
			}
			if *printed != tc.printed {
				t.Fatalf("the command's flag-error func ran %d time(s), want %d", *printed, tc.printed)
			}
			var notExist *pflag.NotExistError
			if name == "flag API misuse" && !errors.As(err, &notExist) {
				t.Fatalf("the misuse case did not produce pflag's NotExistError (%T): it proves nothing", err)
			}
		})
	}
}

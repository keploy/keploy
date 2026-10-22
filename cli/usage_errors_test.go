package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
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

// A command line keploy cannot parse is answered with what is wrong with it
// and where its help is, not with the command's whole usage text: cobra
// printed every flag of `keploy record` (some 80 lines) under the one line
// that said `unknown flag: --delay`, and the line the user needed scrolled
// away. Cobra prints nothing of its own for a usage error -- the binary's
// main says it, once, with the pointer to `<command> --help` -- whether or
// not the tree silences cobra (OSS's root does; the enterprise binary's does
// not). An error a command returns while it runs is left as it was.
func TestAUsageErrorPrintsNoUsageText(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		usage bool
	}{
		"unknown flag":        {[]string{"run", "--nope"}, true},
		"invalid flag value":  {[]string{"run", "--delay", "abc"}, true},
		"too many arguments":  {[]string{"diff", "a", "b", "c"}, true},
		"the run's own error": {[]string{"fails"}, false},
		// Its arguments were checked, and were right: nothing is silenced.
		"the run's own error after its arguments passed": {[]string{"check", "a"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			// Silences nothing, as the enterprise binary's root does not.
			root := &cobra.Command{Use: "keploy"}
			run := &cobra.Command{Use: "run", RunE: func(*cobra.Command, []string) error { return nil }}
			run.Flags().Int("delay", 0, "seconds to wait")
			diff := &cobra.Command{Use: "diff", Args: cobra.MaximumNArgs(2), RunE: func(*cobra.Command, []string) error { return nil }}
			fails := &cobra.Command{Use: "fails", RunE: func(*cobra.Command, []string) error { return errors.New("boom") }}
			check := &cobra.Command{Use: "check", Args: cobra.MaximumNArgs(2), RunE: func(*cobra.Command, []string) error { return errors.New("boom") }}
			root.AddCommand(run, diff, fails, check)
			TagUsageErrors(root)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)

			cmd, err := root.ExecuteC()
			if err == nil || utils.IsUsageError(err) != tc.usage {
				t.Fatalf("err = %v, want a usage error: %v", err, tc.usage)
			}
			if !tc.usage {
				// Unchanged: cobra says the run's error, and its usage, as before.
				if !strings.Contains(out.String(), "Error: boom") || !strings.Contains(out.String(), "Usage:") {
					t.Fatalf("a run's own error is no longer printed as it was: %q", out.String())
				}
				return
			}
			if out.Len() != 0 {
				t.Fatalf("cobra printed %q for a usage error; the binary's main says it", out.String())
			}
			if !cmd.SilenceErrors || !cmd.SilenceUsage {
				t.Fatalf("%s: SilenceErrors=%v SilenceUsage=%v, want both: the binary's main reads them to know it must say the error",
					cmd.CommandPath(), cmd.SilenceErrors, cmd.SilenceUsage)
			}
		})
	}
}

// The real command tree, end to end: the first command of the docs' and the
// samples' walkthrough, `keploy record ... --delay 10`, is refused with the
// one line that says why, and no usage text.
func TestRecordGivenAnUnknownFlagSaysSoWithoutItsUsage(t *testing.T) {
	stdout := captureStdout(t)
	root := newRealRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"record", "-c", "docker compose up", "--container-name", "api", "--delay", "10"})

	cmd, err := root.ExecuteC()
	if !utils.IsUsageError(err) || cmd.Name() != "record" {
		t.Fatalf("got %v from %q, want record's usage error", err, cmd.CommandPath())
	}
	said := stdout() + out.String()
	if !strings.Contains(said, "unknown flag: --delay") {
		t.Fatalf("the error was not said: %q", said)
	}
	for _, usage := range []string{"Usage:", "Flags:", "--container-name string"} {
		if strings.Contains(said, usage) {
			t.Fatalf("record's usage text (%q) was printed for an unknown flag:\n%s", usage, said)
		}
	}
}

// captureStdout points os.Stdout -- where the configurator's flag-error func
// prints, through log.PrimarySink -- at a file until the returned func reads it.
// log.PrimarySink reads os.Stdout on each call (utils/log's
// TestPrimarySinkResolvesStdoutLazily pins that); the swap is process-wide, so
// a test that uses it must not run in parallel with one that prints.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = f
	restore := func() {
		os.Stdout = saved
		_ = f.Close()
	}
	t.Cleanup(restore)
	return func() string {
		restore()
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

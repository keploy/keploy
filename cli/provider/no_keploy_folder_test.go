package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const noKeployFolderChild = "KEPLOY_TEST_NO_KEPLOY_FOLDER_CHILD"

// `keploy test` with no keploy folder has nothing to run, and says so. It used
// to say so and then call os.Exit(1) from inside ValidateFlags, which ends the
// process there: no deferred function runs -- main's own (the log file, the
// profiles, the debug-file flush), and those of any build wrapping this
// configurator, such as the one that writes a run's result for the editor
// that launched it. The run then looked like a crash rather than an answer.
// So it returns the refusal like every other one, with the same words, and
// without cobra's usage dump or a "failed to validate flags" ERROR burying
// them: nothing about the flags was wrong. That holds through a copy of the
// command too, which is how a build wrapping this configurator validates its
// own command under another name, and through Validate, which is how
// `keploy test` does.
//
// Checked in a child process, because what is under test is whether the call
// returns at all: an os.Exit here would end the test binary, not fail a test.
func TestValidateFlags_TestWithNoKeployFolderReturnsInsteadOfExiting(t *testing.T) {
	if os.Getenv(noKeployFolderChild) == "1" {
		validateTestWithNoKeployFolder(t)
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	child.Env = append(os.Environ(), noKeployFolderChild+"=1")
	raw, err := child.CombinedOutput()
	out := withoutLogo(string(raw))
	if !strings.Contains(out, "ValidateFlags returned:") {
		t.Fatalf("ValidateFlags never returned -- it ended the process (%v), so no deferred function ran:\n%s", err, out)
	}
	if err != nil {
		t.Fatalf("ValidateFlags returned, but the check of what it returned failed (%v):\n%s", err, out)
	}
}

// withoutLogo drops the colour-coded logo ValidateFlags prints, which is the
// bulk of the child's output and none of what a failure needs.
func withoutLogo(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "\x1b[38;5;") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func validateTestWithNoKeployFolder(t *testing.T) {
	for _, tc := range []struct {
		name string
		// copied passes ValidateFlags a copy of the command cobra runs, the
		// way a build wrapping this configurator does to validate its own
		// command under another name (`replay` as `test`, say). What the
		// refusal sets on that copy never reaches the command cobra reports
		// on.
		copied bool
		// validate runs the whole of Validate, as `keploy test`'s PreRunE
		// does, rather than ValidateFlags alone, as a wrapping build does.
		validate bool
	}{
		{"its own command", false, false},
		{"a copy of its command", true, false},
		{"through Validate", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			savedFound := IsConfigFileFound
			t.Cleanup(func() { IsConfigFileFound = savedFound; viper.Reset() })
			// The messages are compared as text; keep them free of colour codes.
			savedAnsi := models.IsAnsiDisabled
			models.IsAnsiDisabled = true
			t.Cleanup(func() { models.IsAnsiDisabled = savedAnsi })

			core, logs := observer.New(zapcore.InfoLevel)
			c := NewCmdConfigurator(zap.New(core), config.New())
			var validateErr error
			test := &cobra.Command{
				Use: "test",
				PreRunE: func(cmd *cobra.Command, _ []string) error {
					if tc.copied {
						renamed := *cmd
						cmd = &renamed
					}
					if tc.validate {
						validateErr = c.Validate(context.Background(), cmd)
					} else {
						if err := c.PreProcessFlags(cmd); err != nil {
							t.Fatalf("PreProcessFlags: %v", err)
						}
						validateErr = c.ValidateFlags(context.Background(), cmd)
					}
					fmt.Printf("ValidateFlags returned: %v\n", validateErr)
					return validateErr
				},
				RunE: func(*cobra.Command, []string) error {
					t.Error("a `keploy test` with no keploy folder ran")
					return nil
				},
			}
			if err := c.AddFlags(test); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			root := &cobra.Command{Use: "keploy"}
			root.AddCommand(test)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			// An empty project: no keploy folder. Docker mode, as in
			// TestEveryCommandRefusesAFileAtTheKeployFolder, so that the
			// platform and permission checks before this one have nothing to
			// say on any OS.
			project := t.TempDir()
			root.SetArgs([]string{"test", "-c", "docker compose up", "--container-name", "app",
				"--path", project, "--config-path", t.TempDir()})

			err := root.Execute()

			if err == nil {
				t.Fatal("a `keploy test` with no keploy folder was let through")
			}
			if err != validateErr {
				t.Fatalf("the run failed on something other than the refusal: %v", err)
			}
			// A wrapping build tells this refusal from a flag error by it.
			if !errors.Is(err, ErrNoTestSets) {
				t.Errorf("the refusal is not ErrNoTestSets: %v", err)
			}
			if want := filepath.Join(project, "keploy"); !strings.Contains(filepath.ToSlash(err.Error()), filepath.ToSlash(want)) {
				t.Errorf("the refusal does not name the missing folder %s: %v", want, err)
			}
			if strings.Contains(out.String(), "Usage:") {
				t.Errorf("cobra printed the usage dump over the refusal -- nothing about the flags was wrong:\n%s", out.String())
			}
			said := map[string]bool{}
			for _, e := range logs.All() {
				said[e.Message] = true
			}
			for _, want := range []string{
				"No test-sets found. Please record testcases using keploy record command",
				`Example: keploy record -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`,
			} {
				if !said[want] {
					t.Errorf("the user is no longer told %q; logged: %v", want, logs.All())
				}
			}
			// Those two lines are the whole answer: record first. An ERROR on
			// top of them -- "failed to validate flags" -- sends the user to
			// flags that were fine, and was never printed before the refusal
			// was returned rather than exited on.
			if failed := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(failed) != 0 {
				t.Errorf("a `keploy test` with nothing recorded logged an error over the answer: %v", failed)
			}
		})
	}
}

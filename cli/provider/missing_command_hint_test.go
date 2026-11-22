package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keploy/shlex"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// `keploy record` or `keploy test` given no application command is told, once,
// what is missing, and shown a command line of its own to run: the command the
// user ran, starting an application the way keploy runs here (natively, or in
// docker when keploy itself runs in a container).
//
// ValidateFlags' error for it was built anew on each call, so Validate's `err
// == c.noCommandError()` never held and no hint was printed at all. Once it
// was, a native user was shown RootExamples, which are docker run lines only,
// and in docker a user of record was to be shown a `keploy test ... --delay 6`
// line. The error was said twice, as itself and again as "failed to validate
// flags", and cobra printed record's whole usage text under both, which says
// nothing about -c that the hint does not and scrolled it out of view.
func TestARecordOrTestGivenNoCommandIsShownALineOfItsOwn(t *testing.T) {
	for _, command := range []string{"record", "test"} {
		for _, inDocker := range []bool{false, true} {
			t.Run(command+map[bool]string{false: "", true: " in docker"}[inDocker], func(t *testing.T) {
				original := NativeCommandSupported
				t.Cleanup(func() { NativeCommandSupported = original })
				RegisterNativeCommandSupport(func(string, string) bool { return true })
				viper.Reset()
				t.Cleanup(viper.Reset)
				t.Cleanup(log.TestOnlyResetSink)
				dir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(dir, "keploy"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Chdir(dir)

				cfg := config.New()
				cfg.InDocker = inDocker
				core, logs := observer.New(zapcore.InfoLevel)
				c := NewCmdConfigurator(zap.New(core), cfg)
				cmd := &cobra.Command{Use: command}
				if err := c.AddFlags(cmd); err != nil {
					t.Fatalf("AddFlags: %v", err)
				}
				if err := cmd.ParseFlags([]string{"--config-path", dir}); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}
				cmd.SetContext(context.Background())

				err := c.Validate(context.Background(), cmd)
				if !errors.Is(err, errNoCommand) {
					t.Fatalf("Validate = %v, want the missing-command error", err)
				}

				// The hint is the example said after the error. (In docker the
				// buildDelay hint, which has an example of its own, comes
				// first.)
				var said, hints []string
				errorsSaid := 0
				for _, e := range logs.All() {
					said = append(said, e.Level.String()+" "+e.Message)
					if e.Level >= zapcore.ErrorLevel {
						errorsSaid++
					}
					if line, ok := strings.CutPrefix(e.Message, LogExample("")); ok && errorsSaid > 0 {
						hints = append(hints, line)
					}
				}
				if errorsSaid != 1 {
					t.Errorf("the missing command was said as %d errors, want 1:\n%s", errorsSaid, strings.Join(said, "\n"))
				}
				if len(hints) != 1 {
					t.Fatalf("%d example lines said after the error, want 1:\n%s", len(hints), strings.Join(said, "\n"))
				}
				wantKind := utils.Native
				if inDocker {
					wantKind = utils.DockerRun
				}
				ran, app := exampleCommand(t, hints[0])
				if ran != command {
					t.Errorf("a user of `keploy %s` was shown a `keploy %s` line: %q", command, ran, hints[0])
				}
				if got := utils.FindDockerCmd(app); got != wantKind {
					t.Errorf("shown %q, whose -c is a %q command; keploy here runs a %q one", hints[0], got, wantKind)
				}
				// The error comes back from PreRunE, which cobra answers with
				// the command's usage text unless told not to.
				if !cmd.SilenceUsage {
					t.Errorf("cobra is left to print `keploy %s`'s usage text under the hint", command)
				}
			})
		}
	}
}

// exampleCommand is the keploy command line names (`record` in `keploy record
// -c ...`) and the application command its -c gives.
func exampleCommand(t *testing.T, line string) (command, app string) {
	t.Helper()
	words, err := shlex.Split(line)
	if err != nil || len(words) < 2 || words[0] != "keploy" {
		t.Fatalf("%q is not a keploy command line (%v)", line, err)
	}
	for i, w := range words[:len(words)-1] {
		if w == "-c" || w == "--command" {
			app = words[i+1]
		}
	}
	if app == "" {
		t.Fatalf("%q gives no -c", line)
	}
	return words[1], app
}

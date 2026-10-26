package cli

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
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestEveryPrintedExampleIsACommandLineKeployAccepts checks the lines the
// hint helpers return; these check the lines a run prints, from where it
// prints them. A hint is printed for one reason (a missing -c, a short
// --buildDelay), and the line under that reason has to be one keploy accepts
// (parseExample), of the command the user ran, starting an application the
// way the user's starts. Each of these went wrong once: the no-test-sets hint
// told every docker and compose user `keploy record -c "docker run ..."
// --delay 6`, which record refuses; the missing -c hint showed native users
// docker run lines; the buildDelay and missing --container-name hints
// showed a user of `keploy test` a `keploy record` line; and the delay hint of
// keploy in docker showed a compose user a docker run line.

// hintCase is one run of `keploy <command> <args>` and the hint it has to
// print, under the message that says why.
type hintCase struct {
	name      string
	command   string // record or test
	args      []string
	inDocker  bool // keploy runs in a container (keploy.yml's inDocker)
	keployDir bool // the project has a keploy folder
	reason    string
	failure   string        // the error the run stops with, if it stops
	kind      utils.CmdType // the kind of application the line's -c starts; empty: not asked
	raises    string        // a flag the hint is there to raise ...
	above     uint64        // ... above this, the value that prints the hint
}

const (
	noCommand     = "missing required -c flag or appCmd in config file"
	noContainer   = "missing required --container-name flag or containerName in config file"
	composeRun    = "docker compose up"
	dockerRunApp  = "docker run --name app -p 8080:8080 myimage"
	nativeRunApp  = "./app serve"
	containerName = "app"
)

func TestEveryHintARecordOrTestPrintsIsALineOfTheCommandRun(t *testing.T) {
	compose := []string{"-c", composeRun, "--container-name", containerName}
	var cases []hintCase
	for _, command := range []string{"record", "test"} {
		cases = append(cases,
			hintCase{name: "no -c", command: command, keployDir: true,
				reason: noCommand, failure: noCommand, kind: utils.Native},
			hintCase{name: "no -c, in docker", command: command, inDocker: true, keployDir: true,
				reason: noCommand, failure: noCommand, kind: utils.DockerRun},
			// ValidateFlags: buildDelay <= 30 in docker.
			hintCase{name: "default buildDelay, in docker", command: command, args: compose, inDocker: true, keployDir: true,
				reason: "buildDelay is set to", kind: utils.DockerCompose, raises: "build-delay", above: 30},
			hintCase{name: "compose with no --container-name, in docker", command: command, args: []string{"-c", composeRun},
				inDocker: true, keployDir: true, reason: "Couldn't find containerName", failure: noContainer, kind: utils.DockerCompose},
		)
	}
	// ValidateFlags: delay <= 5. Natively the hint is test's own Example.
	cases = append(cases,
		// In docker its line starts the application as a container, the way
		// the user's starts: a compose line for a compose application, a
		// docker run line for a docker run one (a compose user was shown
		// docker run). A user of keploy in docker is not shown the native
		// Example (`keploy test -c "/path/to/user/app"`).
		hintCase{name: "default delay, compose, in docker", command: "test", args: compose, inDocker: true, keployDir: true,
			reason: "Delay is set to", kind: utils.DockerCompose, raises: "delay", above: 5},
		hintCase{name: "default delay, docker run, in docker", command: "test", args: []string{"-c", dockerRunApp}, inDocker: true, keployDir: true,
			reason: "Delay is set to", kind: utils.DockerRun, raises: "delay", above: 5},
		hintCase{name: "default delay", command: "test", args: []string{"-c", nativeRunApp}, keployDir: true,
			reason: "Delay is set to", kind: utils.Native, raises: "delay", above: 5},
	)

	for _, tc := range cases {
		t.Run(tc.command+"/"+tc.name, func(t *testing.T) {
			project := t.TempDir()
			core, logs := observer.New(zapcore.InfoLevel)
			cmd, cobraSaid, err := runUpToTheRun(t, core, project, tc)
			if tc.failure == "" {
				if err != nil {
					t.Fatalf("`keploy %s %s` failed before the run: %v", tc.command, strings.Join(tc.args, " "), err)
				}
			} else {
				if err == nil || err.Error() != tc.failure {
					t.Fatalf("`keploy %s %s` = %v, want %q", tc.command, strings.Join(tc.args, " "), err, tc.failure)
				}
			}
			var said []string
			for _, e := range logs.All() {
				said = append(said, e.Message)
			}
			line := hintUnder(t, said, tc.reason, provider.LogExample(""))
			checkHint(t, tc, line)
			// A hint is printed for the user to read: not under cobra's whole
			// usage text, which says nothing it does not (some 80 lines).
			// Every failure here is a refusal of Validate's (PreRunE), which
			// no usage text follows (TestWhatValidateRefusesIsSaidWithoutTheUsageText),
			// and a run that goes on prints none.
			if strings.Contains(cobraSaid, "Usage:") {
				t.Errorf("`keploy %s` printed its usage text under the hint:\n%s", cmd.Name(), cobraSaid)
			}
		})
	}
}

// What Validate refuses is said once, by keploy's log, and cobra's usage text
// does not follow it. Validate runs as PreRunE, after cobra has parsed the
// command line, so its refusal is never about how the command was typed: it is
// about the values (--must-pass with no --test-sets), keploy.yml, or this
// machine. AddFlags silences cobra's own error line on every command, so the
// log is the only place the refusal is said. cobra still printed the command's
// usage text under it: 75 lines for test, 46 for record in docker, which said
// nothing about the refusal and scrolled the line that did out of view. The
// log is read as keploy's console encoder prints it, so a refusal carried in a
// line's error field ("failed to validate flags {"error": ...}") counts, as
// it is on the terminal. A command Validate lets through is left as it was:
// whatever its run returns gets cobra's usage text as before.
func TestWhatValidateRefusesIsSaidWithoutTheUsageText(t *testing.T) {
	for _, tc := range []struct {
		hintCase
		refusal string // the error Validate returns; empty: it lets the command through
	}{
		{hintCase: hintCase{name: "test --must-pass with no --test-sets", command: "test",
			args: []string{"-c", nativeRunApp, "--must-pass"}, keployDir: true},
			refusal: "--test-sets flag must be set to use --must-pass=true"},
		{hintCase: hintCase{name: "compose with no --container-name, in docker", command: "record",
			args: []string{"-c", composeRun}, inDocker: true, keployDir: true},
			refusal: noContainer},
		{hintCase: hintCase{name: "no -c", command: "record", keployDir: true},
			refusal: noCommand},
		{hintCase: hintCase{name: "let through", command: "test",
			args: []string{"-c", nativeRunApp}, keployDir: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var printed bytes.Buffer
			core := zapcore.NewCore(log.NewANSIConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(&printed), zapcore.InfoLevel)
			cmd, cobraSaid, err := runUpToTheRun(t, core, t.TempDir(), tc.hintCase)
			line := "`keploy " + strings.TrimSpace(tc.command+" "+strings.Join(tc.args, " ")) + "`"
			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("%s failed before the run: %v", line, err)
				}
				if cmd.SilenceUsage {
					t.Errorf("%s: Validate let it through, and silenced cobra's usage text for whatever its run returns", line)
				}
				return
			}
			if err == nil || err.Error() != tc.refusal {
				t.Fatalf("%s = %v, want %q", line, err, tc.refusal)
			}
			said := printed.String() + cobraSaid
			if n := strings.Count(said, tc.refusal); n != 1 {
				t.Errorf("%s: %q printed %d time(s), want once:\n%s", line, tc.refusal, n, said)
			}
			if strings.Contains(cobraSaid, "Usage:") {
				t.Errorf("%s printed its usage text under the refusal:\n%s", line, cobraSaid)
			}
		})
	}
}

// `keploy test` with nothing recorded tells the user to record, with a
// `keploy record` line for the application the test was to start: a compose
// user was shown `keploy record -c "docker run ..." --delay 6`. It says so and
// exits the process (status 1), so each run is a process of its own: this
// test binary, run again for that one case.
func TestTheNoTestSetsHintRecordsTheKindOfApplicationTheTestStarts(t *testing.T) {
	const childEnv = "KEPLOY_TEST_NO_TEST_SETS_CASE"
	cases := map[string]hintCase{
		"native":     {args: []string{"-c", nativeRunApp}, kind: utils.Native},
		"docker run": {args: []string{"-c", dockerRunApp}, kind: utils.DockerRun},
		"compose":    {args: []string{"-c", composeRun, "--container-name", containerName}, kind: utils.DockerCompose},
	}
	for name, tc := range cases {
		tc.name, tc.command, tc.reason = name, "test", "No test-sets found"
		cases[name] = tc
	}

	if name, ok := os.LookupEnv(childEnv); ok {
		// The child: the run, its log on stderr as it is said (os.Exit
		// leaves nothing to flush).
		tc, known := cases[name]
		if !known {
			t.Fatalf("no case %q", name)
		}
		enc := zap.NewProductionEncoderConfig()
		enc.TimeKey, enc.LevelKey, enc.NameKey, enc.CallerKey, enc.FunctionKey, enc.StacktraceKey = "", "", "", "", "", ""
		stderr := zapcore.NewCore(zapcore.NewConsoleEncoder(enc), zapcore.Lock(os.Stderr), zapcore.InfoLevel)
		_, _, err := runUpToTheRun(t, stderr, t.TempDir(), tc)
		// Not t.Fatal, whose status (1) is the one asked for.
		fmt.Fprintf(os.Stderr, "the run went on past the missing test sets (%v)\n", err)
		os.Exit(3)
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			args := []string{"-test.run=^" + t.Name()[:strings.IndexByte(t.Name(), '/')] + "$", "-test.count=1"}
			// The child is stopped with this test (CommandContext), and
			// stops itself by this run's -timeout if it has to.
			if deadline, ok := t.Deadline(); ok {
				args = append(args, "-test.timeout="+time.Until(deadline).String())
			}
			child := exec.CommandContext(t.Context(), os.Args[0], args...)
			// The child's t.TempDir()s are made under a dir of this test's,
			// removed with it: the child's own cleanup never runs, the
			// process exits from inside the run. t.TempDir makes its dir in
			// GOTMPDIR when that is set, else in os.TempDir() (TMPDIR; TMP
			// or TEMP on Windows).
			tmp := t.TempDir()
			child.Env = append(os.Environ(), childEnv+"="+name,
				"GOTMPDIR="+tmp, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
			var out bytes.Buffer
			child.Stdout, child.Stderr = &out, &out
			err := child.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("`keploy test %s` with no test sets: %v, want exit status 1; it said:\n%s", strings.Join(tc.args, " "), err, out.String())
			}
			line := hintUnder(t, strings.Split(out.String(), "\n"), tc.reason, "Example: ")
			tc.command = "record"
			checkHint(t, tc, line)
		})
	}
}

// runUpToTheRun runs `keploy <tc.command> <tc.args>` through the real command
// tree -- Root, the command's flags, cobra's parse and its PreRunE (Validate,
// ValidateFlags) -- with the run itself replaced, in a project of its own. It
// returns the command, what cobra printed, and the error the command line
// failed with.
func runUpToTheRun(t *testing.T, core zapcore.Core, project string, tc hintCase) (*cobra.Command, string, error) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Cleanup(log.TestOnlyResetSink)
	found := provider.IsConfigFileFound
	t.Cleanup(func() { provider.IsConfigFileFound = found })
	native := provider.NativeCommandSupported
	t.Cleanup(func() { provider.NativeCommandSupported = native })
	provider.RegisterNativeCommandSupport(func(string, string) bool { return true })
	// No keploy.yml, so nothing but the flags decides the run; or one that
	// says keploy runs in docker (inDocker, which no flag sets).
	configDir := t.TempDir()
	if tc.inDocker {
		if err := os.WriteFile(filepath.Join(configDir, "keploy.yml"), []byte("inDocker: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if tc.keployDir {
		if err := os.Mkdir(filepath.Join(project, "keploy"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(project)

	root := Root(context.Background(), zap.NewNop(), nil, provider.NewCmdConfigurator(zap.New(core), config.New()))
	cmd, _, err := root.Find([]string{tc.command})
	if err != nil {
		t.Fatalf("no `keploy %s`: %v", tc.command, err)
	}
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	var cobraSaid bytes.Buffer
	root.SetOut(&cobraSaid)
	root.SetErr(&cobraSaid)
	root.SetArgs(append(append([]string{tc.command}, tc.args...),
		"--path", project, "--config-path", configDir))
	_, err = root.ExecuteC()
	return cmd, cobraSaid.String(), err
}

// hintUnder is the first line in said after the one that starts with reason,
// and starts with prefix, with the prefix cut.
func hintUnder(t *testing.T, said []string, reason, prefix string) string {
	t.Helper()
	for i, line := range said {
		if !strings.HasPrefix(strings.TrimSpace(line), reason) {
			continue
		}
		for _, next := range said[i+1:] {
			if hint, ok := strings.CutPrefix(strings.TrimSpace(next), prefix); ok {
				return hint
			}
		}
		break
	}
	t.Fatalf("no %q line said under %q:\n%s", prefix, reason, strings.Join(said, "\n"))
	return ""
}

// checkHint: the line is one keploy accepts, of tc.command, its -c of
// tc.kind, raising tc.raises above tc.above.
func checkHint(t *testing.T, tc hintCase, line string) {
	t.Helper()
	cmd, err := parseExample(line)
	if err != nil {
		t.Fatalf("the hint is not a line keploy accepts: %v", err)
	}
	if cmd.Name() != tc.command {
		t.Errorf("the hint %q runs `%s`, want `keploy %s`", line, cmd.CommandPath(), tc.command)
	}
	if tc.kind != "" {
		app, _ := cmd.Flags().GetString("command")
		if got := utils.FindDockerCmd(app); got != tc.kind {
			t.Errorf("the hint %q starts a %q application, want %q", line, got, tc.kind)
		}
		if tc.kind == utils.DockerRun && utils.ContainerNameFromDockerRun(app) == "" {
			t.Errorf("the hint %q runs a docker container it does not name (--name)", line)
		}
	}
	if tc.raises != "" {
		v, err := cmd.Flags().GetUint64(tc.raises)
		if err != nil {
			t.Fatalf("the hint %q: --%s: %v", line, tc.raises, err)
		}
		if !cmd.Flags().Changed(tc.raises) || v <= tc.above {
			t.Errorf("the hint %q does not raise --%s above %d", line, tc.raises, tc.above)
		}
	}
}

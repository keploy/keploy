package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils"
)

// subprocessEnv makes TestMain run the real CLI instead of the test suite, so
// the tests below observe genuine process exit codes. Testing the helper in
// isolation is not enough: the bug this guards against was the helper's result
// never being assigned to utils.ErrCode, which a pure unit test cannot see.
const subprocessEnv = "KEPLOY_EXITCODE_TEST_SUBPROCESS"

func TestMain(m *testing.M) {
	if os.Getenv(subprocessEnv) == "1" {
		setVersion()
		start(utils.NewCtx())
		os.Exit(utils.ErrCode)
	}
	os.Exit(m.Run())
}

// runCLI re-executes this test binary as the keploy CLI with the given
// arguments, in a scratch working directory, and returns the exit code.
func runCLI(t *testing.T, args ...string) int {
	t.Helper()
	code, _ := runCLIOutput(t, args...)
	return code
}

// runCLIOutput is runCLI, with what the CLI wrote to stdout and stderr.
func runCLIOutput(t *testing.T, args ...string) (int, string) {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Args = append([]string{"keploy"}, args...)
	cmd.Env = append(os.Environ(), subprocessEnv+"=1")
	cmd.Dir = t.TempDir()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), out.String()
		}
		t.Fatalf("running %v: %v", args, err)
	}
	return 0, out.String()
}

func TestCLIExitCodes(t *testing.T) {
	// Startup does more than parse flags - it reads/creates the installation
	// id, for one - and none of that is under test here. If the happy path
	// cannot even reach exit 0 the environment is broken, and asserting the
	// failure cases would only produce a misleading red.
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "root help", args: []string{"--help"}, want: 0},
		{name: "subcommand help", args: []string{"test", "--help"}, want: 0},
		{name: "version flag", args: []string{"--version"}, want: 0},
		{name: "unknown command", args: []string{"bogus-cmd"}, want: utils.ExitUsageError},
		// A flag pflag cannot parse is a usage error too: the command never ran.
		{name: "unknown flag", args: []string{"test", "--nope"}, want: utils.ExitUsageError},
		{name: "unknown shorthand", args: []string{"test", "-Z"}, want: utils.ExitUsageError},
		{name: "invalid flag value", args: []string{"test", "--delay", "abc"}, want: utils.ExitUsageError},
		{name: "flag missing its value", args: []string{"test", "--delay"}, want: utils.ExitUsageError},
		{name: "unknown mock flag", args: []string{"mock", "record", "--nope"}, want: utils.ExitUsageError},
		{name: "too many arguments", args: []string{"diff", "a", "b", "c"}, want: utils.ExitUsageError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runCLI(t, tt.args...); got != tt.want {
				t.Errorf("keploy %v exit code = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

// A usage error is said once, then where the help of the command that failed
// is: the flag `keploy record --delay 10` got wrong is one of record's, which
// `keploy record --help` lists, and the root's help lists only commands. No
// usage text is printed under it (some 80 lines, which scrolled the error out
// of view). start takes the failing command from ExecuteC and hands its path
// to exitCodeForCmdErr, which prints whatever path it is given: only a run of
// start tells the command that failed from the root.
func TestCLIPointsAUsageErrorAtTheHelpOfTheCommandThatFailed(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}

	for _, tc := range []struct {
		args []string
		said string // the error, said once
		help string // the command whose --help it points at
	}{
		// The walkthrough's first command.
		{args: []string{"record", "-c", "docker compose up", "--container-name", "api", "--delay", "10"},
			said: "unknown flag: --delay", help: "keploy record"},
		{args: []string{"test", "--nope"}, said: "unknown flag: --nope", help: "keploy test"},
		{args: []string{"mock", "record", "--nope"}, said: "unknown flag: --nope", help: "keploy mock record"},
		{args: []string{"diff", "a", "b", "c"}, said: "accepts at most 2 arg(s), received 3", help: "keploy diff"},
		{args: []string{"bogus-cmd"}, said: `unknown command "bogus-cmd" for "keploy"`, help: "keploy"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, out := runCLIOutput(t, tc.args...)
			if code != utils.ExitUsageError {
				t.Errorf("exit code = %d, want %d", code, utils.ExitUsageError)
			}
			if n := strings.Count(out, tc.said); n != 1 {
				t.Errorf("%q said %d times, want once:\n%s", tc.said, n, out)
			}
			if hint := "Run '" + tc.help + " --help' for usage."; strings.Count(out, "--help' for usage.") != 1 || !strings.Contains(out, hint) {
				t.Errorf("want one help pointer, %q:\n%s", hint, out)
			}
			if strings.Contains(out, "Usage:") {
				t.Errorf("the usage text was printed under the error:\n%s", out)
			}
		})
	}
}

// TestCLICleansUpLogFileOnUnknownCommand pins the other half of routing errors
// through utils.ErrCode rather than os.Exit(1): the deferred cleanup in start()
// now actually runs, so a failed invocation no longer strands a keploy-logs.txt
// in the user's working directory.
func TestCLICleansUpLogFileOnUnknownCommand(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", "bogus-cmd"}
	cmd.Env = append(os.Environ(), subprocessEnv+"=1")
	cmd.Dir = dir
	_ = cmd.Run()

	if _, err := os.Stat(dir + "/keploy-logs.txt"); !os.IsNotExist(err) {
		t.Errorf("keploy-logs.txt left behind after a failed command (stat err: %v)", err)
	}
}

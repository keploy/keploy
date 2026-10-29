package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils"
)

// subprocessEnv makes TestMain run the real CLI instead of the test suite, so
// the tests below observe genuine process exit codes. Testing the helper in
// isolation is not enough: the bug this guards against was the helper's result
// never being assigned to utils.ErrCode, which a pure unit test cannot see.
const subprocessEnv = "KEPLOY_EXITCODE_TEST_SUBPROCESS"

// childWouldSudo is the exit status of a child refused because start() would
// re-execute it through sudo. No code in the exit-code contract is 99.
const childWouldSudo = 99

func TestMain(m *testing.M) {
	if os.Getenv(subprocessEnv) == "1" {
		// start() re-executes keploy through sudo before cobra parses
		// anything when the -c is a docker command and the user is not root
		// (utils.ShouldReexecWithSudo). Here that would run this test binary
		// as root, with the caller's environment, where sudo needs no
		// password; ask for a password where it does, and block the test;
		// and fail where there is no sudo. The result would depend on the
		// machine's sudo, not on keploy, so a case that would do it is
		// refused, the same way everywhere: give it a native -c.
		if utils.ShouldReexecWithSudo() {
			fmt.Fprintf(os.Stderr, "refused: keploy %s would re-execute this test binary through sudo; give the case a native -c\n", strings.Join(os.Args[1:], " "))
			os.Exit(childWouldSudo)
		}
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
	// A CLI that cannot even print its help is broken, and that is a failure
	// of this test, not a reason to skip it: skipped, it would go green.
	if code, out := runCLIOutput(t, "--help"); code != 0 {
		t.Fatalf("keploy --help exited %d, want 0; the CLI does not start:\n%s", code, out)
	}

	for _, tc := range []struct {
		args []string
		said string // the error, said once
		help string // the command whose --help it points at
	}{
		// The walkthrough's first command, with a native -c: its docker one,
		// `-c "docker compose up"`, would re-execute the child through sudo
		// (TestMain refuses that). The docker line itself is run in-process,
		// where nothing re-executes, by cli's
		// TestRecordGivenAnUnknownFlagSaysSoWithoutItsUsage.
		{args: []string{"record", "-c", "./app", "--container-name", "api", "--delay", "10"},
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

// TestMain's refusal is what keeps every case above off sudo, the same way on
// every machine: a child given a docker -c, run by a user who is not root, is
// refused before start() runs, and so before anything re-executes. The line is
// the walkthrough's. Were the refusal gone, start() would re-execute the child
// through sudo first, since its sudo check runs before cobra parses the line;
// only the re-executed child would stop at --delay, which record has no flag
// for. It would never start the application, but it would go through sudo.
func TestCLIChildThatWouldReexecThroughSudoIsRefused(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("start() re-executes through sudo only on Linux, for a user who is not root")
	}
	code, out := runCLIOutput(t, "record", "-c", "docker compose up", "--container-name", "api", "--delay", "10")
	if code != childWouldSudo || !strings.Contains(out, "would re-execute this test binary through sudo") {
		t.Errorf("exit code = %d, want %d (refused), and the refusal said:\n%s", code, childWouldSudo, out)
	}
}

// TestCLICleansUpLogFileOnUnknownCommand pins the other half of routing errors
// through utils.ErrCode rather than os.Exit(1): the deferred cleanup in start()
// now actually runs, so a failed invocation leaves no log behind -- not in the
// temp directory, where the log is, and nothing in the working directory,
// where keploy-logs.txt used to be.
func TestCLICleansUpLogFileOnUnknownCommand(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}

	dir, tmp := t.TempDir(), t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", "bogus-cmd"}
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+tmp)
	cmd.Dir = dir
	_ = cmd.Run()

	for _, d := range []string{dir, tmp} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("a failed command left %s behind in %s", e.Name(), d)
		}
	}
}

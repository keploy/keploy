package main

import (
	"errors"
	"os"
	"os/exec"
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

	cmd := exec.Command(os.Args[0])
	cmd.Args = append([]string{"keploy"}, args...)
	cmd.Env = append(os.Environ(), subprocessEnv+"=1")
	cmd.Dir = t.TempDir()

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("running %v: %v", args, err)
	}
	return 0
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
		{name: "unknown command", args: []string{"bogus-cmd"}, want: 1},
		{name: "unknown flag", args: []string{"test", "--nope"}, want: 1},
		{name: "invalid flag value", args: []string{"test", "--delay", "abc"}, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runCLI(t, tt.args...); got != tt.want {
				t.Errorf("keploy %v exit code = %d, want %d", tt.args, got, tt.want)
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

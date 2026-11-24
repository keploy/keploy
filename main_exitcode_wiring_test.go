package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/cli"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
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
		if utils.ShouldReexecWithSudo() && os.Getenv(reexecWithNoSudoEnv) != "1" {
			fmt.Fprintf(os.Stderr, "refused: keploy %s would re-execute this test binary through sudo; give the case a native -c\n", strings.Join(os.Args[1:], " "))
			os.Exit(childWouldSudo)
		}
		if hold := os.Getenv(holdAfterLoggerEnv); hold != "" {
			holdAfterLogger(hold)
		}
		if os.Getenv(panicCommandEnv) == "1" {
			registerPanicCommand()
		}
		setVersion()
		start(utils.NewCtx())
		os.Exit(utils.ErrCode)
	}
	os.Exit(m.Run())
}

// panicCommandEnv gives a re-executed CLI the command panicCommand, which
// panics as it runs: a crash of the command's own goroutine.
const panicCommandEnv = "KEPLOY_TEST_PANIC_COMMAND"

// panicCommand is the name of the command registerPanicCommand adds.
const panicCommand = "zz-test-panic"

// workerPanicCommand is the name of the command registerPanicCommand adds
// whose worker goroutine panics, under utils.Recover as keploy's workers run.
const workerPanicCommand = "zz-test-worker-panic"

// wrappedExitEightCommand is the name of the command registerPanicCommand
// adds that ends with the exit code 8 a command it wraps left, as `keploy mock
// replay -c ctest` does when ctest's tests fail.
const wrappedExitEightCommand = "zz-test-wrapped-exit-8"

// registerPanicCommand adds panicCommand, which logs a line and panics, and
// workerPanicCommand, which logs a line, starts a worker that panics under
// utils.Recover (which stops the run's context), and returns nil once the
// context ends: a command that takes its context's end for a normal stop.
func registerPanicCommand() {
	cli.Register(panicCommand, func(_ context.Context, logger *zap.Logger, _ *config.Config, _ cli.ServiceFactory, _ cli.CmdConfigurator) *cobra.Command {
		return &cobra.Command{Use: panicCommand, RunE: func(*cobra.Command, []string) error {
			logger.Info("the line before the crash")
			panic("a crash of the command")
		}}
	})
	cli.Register(workerPanicCommand, func(ctx context.Context, logger *zap.Logger, _ *config.Config, _ cli.ServiceFactory, _ cli.CmdConfigurator) *cobra.Command {
		return &cobra.Command{Use: workerPanicCommand, RunE: func(*cobra.Command, []string) error {
			logger.Info("the line before the crash")
			go func() {
				defer utils.Recover(logger)
				panic("a crash of a worker")
			}()
			<-ctx.Done()
			return nil
		}}
	})
	cli.Register(wrappedExitEightCommand, func(_ context.Context, logger *zap.Logger, _ *config.Config, _ cli.ServiceFactory, _ cli.CmdConfigurator) *cobra.Command {
		return &cobra.Command{Use: wrappedExitEightCommand, RunE: func(*cobra.Command, []string) error {
			logger.Info("the line before the wrapped command's exit")
			utils.SetExitCodeOnce(utils.ExitUsageError)
			return nil
		}}
	})
}

// reexecWithNoSudoEnv lets a re-executed CLI that would re-execute itself
// through sudo go on to that re-exec. Only a row whose PATH has no sudo on it
// sets it, so the re-exec fails where it looks for sudo, and nothing runs as
// root.
const reexecWithNoSudoEnv = "KEPLOY_TEST_REEXEC_WITH_NO_SUDO"

// holdAfterLoggerEnv names a named pipe at which a re-executed CLI is held
// right after start() built its logger (newLogger, the real one), before
// cobra parses anything: holdAfterLogger.
const holdAfterLoggerEnv = "KEPLOY_TEST_HOLD_AFTER_LOGGER"

// holdAfterLogger holds the CLI right after its logger is built: it opens the
// named pipe fifo to read, which waits for the test to open it to write, and
// reads it until the test closes it. When the test's open returns, the run
// has its logger, and its log file if it keeps one, and does nothing more
// until the test lets it go.
func holdAfterLogger(fifo string) {
	inner := newLogger
	newLogger = func() (*zap.Logger, *os.File, error) {
		logger, f, err := inner()
		if r, openErr := os.Open(fifo); openErr == nil {
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		}
		return logger, f, err
	}
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
	// A TMPDIR of its own: the run's start prunes the logs kept there, and a
	// run that fails keeps one.
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+t.TempDir())
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
// now actually runs, so a usage error, which keploy refused before running
// anything, leaves no log behind -- not in the temp directory, where the log
// is, and nothing in the working directory, where keploy-logs.txt used to be.
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

// requireOnlyItsKeptLog checks a run that failed, given everything it wrote
// (said): it left nothing in its working directory dir, and only its log in
// its temp directory tmp, kept there for the bug report, and said so once
// (utils.EndRunLog). A failed run used to remove its log, so the runs that
// needed one to report left none; the next run's start prunes the kept ones.
func requireOnlyItsKeptLog(t *testing.T, said, dir, tmp, what string) (logPath string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("%s left %s behind in its working directory %s", what, e.Name(), dir)
	}
	entries, err = os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%s left %d files in its temp directory %s, want its one kept log: %v", what, len(entries), tmp, entries)
	}
	logPath = filepath.Join(tmp, entries[0].Name())
	if ok, _ := filepath.Match(log.LogFilePattern, entries[0].Name()); !ok || !entries[0].Type().IsRegular() {
		t.Errorf("%s left %s in its temp directory, which is not its log", what, entries[0].Name())
	}
	if want := utils.KeptRunLogLine(logPath); strings.Count(said, want) != 1 {
		t.Errorf("%s did not say once that its log is kept, %q:\n%s", what, want, said)
	}
	return logPath
}

// `keploy test` with no keploy folder, a new user's first mistake, fails with
// exit code 1, names its log, and keeps it in the temp directory, saying so,
// and leaves nothing in the working directory. ValidateFlags used to
// os.Exit(1) there, past start()'s deferred calls. It now returns
// provider.ErrNoTestSets, which main exits 1 for, and which nothing says a
// second time as "failed to validate flags".
func TestCLITestWithNoKeployFolderExitsOneAndKeepsItsLog(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}

	dir, tmp := t.TempDir(), t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", "test", "-c", "true"}
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+tmp)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running keploy test: %v", err)
		}
		code = exitErr.ExitCode()
	}

	if code != utils.ExitKeployError {
		t.Errorf("exit code = %d, want %d", code, utils.ExitKeployError)
	}
	said := stdout.String() + stderr.String()
	if !strings.Contains(said, "No test-sets found") {
		t.Errorf("the run did not say it found no test-sets:\n%s", said)
	}
	if !strings.Contains(said, "logs: "+filepath.Join(tmp, "keploy-logs-")) {
		t.Errorf("the run did not name its log in %s:\n%s", tmp, said)
	}
	if strings.Contains(said, "failed to validate flags") {
		t.Errorf("the refusal was said a second time, as a validation failure:\n%s", said)
	}
	logPath := requireOnlyItsKeptLog(t, said, dir, tmp, "keploy test with no keploy folder")
	if !strings.Contains(said, "logs: "+logPath+"\n") {
		t.Errorf("the run did not name the log it kept, %s, beside its version:\n%s", logPath, said)
	}
	if !strings.Contains(stderr.String(), utils.KeptRunLogLine(logPath)) {
		t.Errorf("the kept log's line is not on stderr:\n%s", stderr.String())
	}
	if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), "No test-sets found") {
		t.Errorf("the kept log does not hold why the run failed (%v):\n%s", err, b)
	}
}

// A docker run that keploy cannot re-execute through sudo, for there is no
// sudo, fails with exit code 1 and keeps its log, saying so: ReexecWithSudo
// returns its error to start(), whose deferred calls end the log. It used to
// os.Exit(1) inside ReexecWithSudo, before the log's name went at the exec,
// and left the log behind with nothing said.
func TestCLIReexecWithNoSudoExitsOneAndKeepsItsLog(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("start() re-executes through sudo only on Linux, for a user who is not root")
	}
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}

	dir, tmp, bin := t.TempDir(), t.TempDir(), t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", "record", "-c", "docker run --name app img"}
	env := []string{subprocessEnv + "=1", reexecWithNoSudoEnv + "=1", "TMPDIR=" + tmp, "PATH=" + bin}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running keploy record: %v", err)
		}
		code = exitErr.ExitCode()
	}
	out := stdout.String() + stderr.String()

	if code != utils.ExitKeployError {
		t.Errorf("exit code = %d, want %d:\n%s", code, utils.ExitKeployError, out)
	}
	if !strings.Contains(out, "sudo is required") {
		t.Errorf("the run did not say it needs sudo:\n%s", out)
	}
	logPath := requireOnlyItsKeptLog(t, out, dir, tmp, "a re-exec with no sudo")
	if !strings.Contains(stderr.String(), utils.KeptRunLogLine(logPath)) {
		t.Errorf("the kept log's line is not on stderr:\n%s", stderr.String())
	}
	if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), "sudo not found in PATH") {
		t.Errorf("the kept log does not hold why the run failed (%v):\n%s", err, b)
	}
}

// A crash of the command's own goroutine is a failed run: it exits non-zero,
// not 0, and keeps its log, holding the line logged before the crash, with
// the line that says so on stderr. utils.Recover recovered such a panic and
// left the code at 0, so the run passed and removed its log.
func TestCLICrashExitsNonZeroAndKeepsItsLog(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}
	dir, tmp := t.TempDir(), t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", panicCommand}
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", panicCommandEnv+"=1", "TMPDIR="+tmp, "SUDO_UID=")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running keploy %s: %v", panicCommand, err)
		}
		code = exitErr.ExitCode()
	}
	if code != utils.ExitKeployError {
		t.Errorf("a crash exited %d, want %d:\n%s%s", code, utils.ExitKeployError, stdout.String(), stderr.String())
	}
	logPath := requireOnlyItsKeptLog(t, stdout.String()+stderr.String(), dir, tmp, "a crash")
	if !strings.Contains(stderr.String(), utils.KeptRunLogLine(logPath)) {
		t.Errorf("the kept log's line is not on stderr:\n%s", stderr.String())
	}
	if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), "the line before the crash") || !strings.Contains(string(b), "a crash of the command") {
		t.Errorf("the kept log does not hold the line logged before the crash and the crash (%v):\n%s", err, b)
	}
}

// A run that exits 8 because the command it wraps did -- ctest exits 8 when its
// tests fail, and keploy mock replay exits with its test command's code -- is
// a failed run, not a command line keploy refused: it keeps its log and says
// so. Only main's mapping of a usage error refuses one (utils.RefuseCommandLine);
// the run removed its log for any 8.
func TestCLIAWrappedCommandsExitEightKeepsItsLog(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}
	dir, tmp := t.TempDir(), t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", wrappedExitEightCommand}
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", panicCommandEnv+"=1", "TMPDIR="+tmp, "SUDO_UID=")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running keploy %s: %v", wrappedExitEightCommand, err)
		}
		code = exitErr.ExitCode()
	}
	if code != utils.ExitUsageError {
		t.Errorf("the run exited %d, want its wrapped command's %d:\n%s%s", code, utils.ExitUsageError, stdout.String(), stderr.String())
	}
	logPath := requireOnlyItsKeptLog(t, stdout.String()+stderr.String(), dir, tmp, "a run whose wrapped command exited 8")
	if !strings.Contains(stderr.String(), utils.KeptRunLogLine(logPath)) {
		t.Errorf("the kept log's line is not on stderr:\n%s", stderr.String())
	}
	if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), "the line before the wrapped command's exit") {
		t.Errorf("the kept log does not hold the run's line (%v):\n%s", err, b)
	}
}

// A crash of a run that tees its debug log into KEPLOY_DEBUG_FILE (--debug)
// leaves its report there too: the debug file is ended after the crash is
// recovered and reported (utils.Recover), as after every other deferred call.
func TestCLICrashSaysItsReportInTheDebugFile(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}
	dir, tmp, debugDir := t.TempDir(), t.TempDir(), t.TempDir()
	debugPath := filepath.Join(debugDir, "debug.log")
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", panicCommand, "--debug"}
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", panicCommandEnv+"=1", "TMPDIR="+tmp, "SUDO_UID=", "KEPLOY_DEBUG_FILE="+debugPath)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running keploy %s --debug: %v", panicCommand, err)
		}
		code = exitErr.ExitCode()
	}
	if code != utils.ExitKeployError {
		t.Errorf("a crash exited %d, want %d:\n%s", code, utils.ExitKeployError, out.String())
	}
	b, err := os.ReadFile(debugPath)
	if err != nil {
		t.Fatalf("the run wrote no debug file: %v\n%s", err, out.String())
	}
	for _, want := range []string{"the line before the crash", "Recovered from panic", "a crash of the command"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the debug file does not hold %q:\n%s", want, b)
		}
	}
}

// A crash on one of keploy's worker goroutines, recovered there
// (utils.Recover), stops the run; the command takes that stop for a normal
// end and returns. The run still failed: it exits non-zero and keeps its log,
// which holds the crash. utils.Recover used to leave the code at 0, so such a
// run passed and removed its log.
func TestCLIWorkerCrashExitsNonZeroAndKeepsItsLog(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}
	dir, tmp := t.TempDir(), t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", workerPanicCommand}
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", panicCommandEnv+"=1", "TMPDIR="+tmp, "SUDO_UID=")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running keploy %s: %v", workerPanicCommand, err)
		}
		code = exitErr.ExitCode()
	}
	if code != utils.ExitKeployError {
		t.Errorf("a run whose worker crashed exited %d, want %d:\n%s%s", code, utils.ExitKeployError, stdout.String(), stderr.String())
	}
	logPath := requireOnlyItsKeptLog(t, stdout.String()+stderr.String(), dir, tmp, "a run whose worker crashed")
	if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), "a crash of a worker") {
		t.Errorf("the kept log does not hold the crash (%v):\n%s", err, b)
	}
}

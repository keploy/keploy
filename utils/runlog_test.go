package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// A run that failed keeps its log; one that worked, and a command line keploy
// refused before running anything (RefuseCommandLine), do not. A run that
// exits 8 without that refusal -- a wrapped test runner's own 8 -- failed, and
// keeps it: the code alone does not say keploy refused the command line.
func TestKeepsRunLog(t *testing.T) {
	t.Cleanup(TestOnlyResetCommandLineRefusal)
	for _, refused := range []bool{false, true} {
		TestOnlyResetCommandLineRefusal()
		if refused {
			RefuseCommandLine()
		}
		for code, want := range map[int]bool{
			0:                     false,
			ExitUsageError:        !refused,
			ExitKeployError:       true,
			ExitPrivilegeRequired: true,
			130:                   true,
		} {
			if got := KeepsRunLog(code); got != want {
				t.Errorf("command line refused: %v: KeepsRunLog(%d) = %v, want %v", refused, code, got, want)
			}
		}
	}
}

// The run's end: after a failed run its log is where it was, closed, and one
// line on stderr names it; after a run that worked, or a usage error, it is
// gone and nothing is said. Either way the run holds no log after it.
func TestEndRunLog(t *testing.T) {
	for _, c := range []struct {
		name    string
		code    int
		refused bool
		keeps   bool
	}{
		{"a failed run", ExitKeployError, false, true},
		{"a run that worked", 0, false, false},
		{"a usage error", ExitUsageError, true, false},
		{"a wrapped command that exited 8", ExitUsageError, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			TestOnlyResetCommandLineRefusal()
			t.Cleanup(TestOnlyResetCommandLineRefusal)
			if c.refused {
				RefuseCommandLine()
			}
			dir := t.TempDir()
			for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(v, dir)
			}
			_, f, err := log.New()
			if err != nil || f == nil {
				t.Fatalf("log.New = %v, %v; want a log file", f, err)
			}
			t.Cleanup(func() { _ = f.Close() })
			saved := LogFile
			t.Cleanup(func() { LogFile = saved })
			wasCrashed := crashed.Load()
			crashed.Store(false)
			t.Cleanup(func() { crashed.Store(wasCrashed) })
			LogFile = f
			path, _ := filepath.Abs(f.Name())

			var stderr bytes.Buffer
			EndRunLog(zap.NewNop(), c.code, &stderr)
			if LogFile != nil {
				t.Error("the run still holds a log after its end")
			}
			_, statErr := os.Lstat(path)
			if c.keeps {
				if statErr != nil {
					t.Errorf("the failed run's log is not where it was: %v", statErr)
				}
				if want := KeptRunLogLine(path) + "\n"; stderr.String() != want {
					t.Errorf("the run said %q, want %q", stderr.String(), want)
				}
				return
			}
			if !os.IsNotExist(statErr) {
				t.Errorf("the log outlived a run that exited %d (lstat: %v)", c.code, statErr)
			}
			if stderr.Len() != 0 {
				t.Errorf("a run that exited %d said %q", c.code, stderr.String())
			}
		})
	}
}

// A run during which keploy crashed (a panic recovered on any goroutine) keeps
// its log, whatever code the command it stopped left: 0 included.
func TestEndRunLogKeepsTheLogOfARunThatCrashed(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, dir)
	}
	_, f, err := log.New()
	if err != nil || f == nil {
		t.Fatalf("log.New = %v, %v; want a log file", f, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	saved := LogFile
	t.Cleanup(func() { LogFile = saved })
	LogFile = f
	wasCrashed := crashed.Load()
	crashed.Store(true)
	t.Cleanup(func() { crashed.Store(wasCrashed) })
	path, _ := filepath.Abs(f.Name())

	var stderr bytes.Buffer
	EndRunLog(nil, 0, &stderr)
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the log of a run that crashed is not where it was: %v", err)
	}
	if want := KeptRunLogLine(path) + "\n"; stderr.String() != want {
		t.Errorf("the run said %q, want %q", stderr.String(), want)
	}
}

// A run that crashed (a panic recovered on any goroutine) exits non-zero
// where it armed no code of its own, and keeps the code it armed.
func TestRunExitCode(t *testing.T) {
	wasCrashed := crashed.Load()
	t.Cleanup(func() { crashed.Store(wasCrashed) })
	for _, c := range []struct {
		crashed    bool
		code, want int
	}{
		{false, 0, 0},
		{false, ExitUsageError, ExitUsageError},
		{true, 0, ExitKeployError},
		{true, 130, 130},
		{true, ExitPrivilegeRequired, ExitPrivilegeRequired},
	} {
		crashed.Store(c.crashed)
		if got := RunExitCode(c.code); got != c.want {
			t.Errorf("crashed=%v: RunExitCode(%d) = %d, want %d", c.crashed, c.code, got, c.want)
		}
	}
}

package utils

import (
	"fmt"
	"io"
	"sync/atomic"

	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// KeepsRunLog says whether a run that exits with code keeps its log
// (log.LogFilePattern) after it ends: a run that failed does, since it is the
// run whose log a bug report needs, and says where (EndRunLog). One that
// worked removes it, and so does a command line keploy refused before
// running anything (RefuseCommandLine), whose whole story the console told.
// The code alone cannot say the latter: a wrapped command's own 8 (a test
// runner that exits 8 when its tests fail) is a failed run like any other.
func KeepsRunLog(code int) bool {
	return code != 0 && (code != ExitUsageError || !refusedCommandLine.Load())
}

// refusedCommandLine is set once the run's main mapped a usage error onto
// ExitUsageError (RefuseCommandLine).
var refusedCommandLine atomic.Bool

// RefuseCommandLine says this run's command line was refused before anything
// ran: a usage error, which main maps onto ExitUsageError. Each binary's main
// calls it where it makes that mapping; a run then removes its log (KeepsRunLog).
func RefuseCommandLine() { refusedCommandLine.Store(true) }

// TestOnlyResetCommandLineRefusal undoes RefuseCommandLine, for a test of the
// process that made it; no run calls it.
func TestOnlyResetCommandLineRefusal() { refusedCommandLine.Store(false) }

// KeptRunLogLine is the line a run that keeps its log says on stderr, naming
// it (EndRunLog). A run fails for the user's tests as well as for keploy, so
// it asks for the log in a report only where keploy is at fault.
func KeptRunLogLine(path string) string {
	return "keploy: this run's log is kept at " + path + "; attach it to a bug report if keploy is at fault"
}

// EndRunLog ends this run's log (LogFile) for a run that exits with code, and
// sets LogFile to nil. A run that keeps it (KeepsRunLog) closes it and leaves
// it where it is (log.KeepLogFile), and says where on w (KeptRunLogLine);
// a later run's start prunes it. Any other closes and removes it
// (log.CloseLogFile). A native agent's log is its CLI's: the agent only
// closes it, however it exits, and the CLI ends it.
func EndRunLog(logger *zap.Logger, code int, w io.Writer) {
	if logger == nil {
		logger = zap.NewNop()
	}
	f := LogFile
	LogFile = nil
	if !KeepsRunLog(RunExitCode(code)) {
		if err := log.CloseLogFile(f); err != nil {
			LogError(logger, err, "Failed to close and remove Keploy Logs")
		}
		return
	}
	path, err := log.KeepLogFile(f)
	if err != nil {
		// The kept log may lack its last lines (a close that failed on a
		// network file system): said, beside the line that names it.
		logger.Warn("failed to close this run's log", zap.Error(err))
	}
	if path != "" {
		_, _ = fmt.Fprintln(w, KeptRunLogLine(path))
	}
}

// crashed is set once a panic is recovered on any goroutine (recovered):
// the run failed, whatever its command's code (Crashed).
var crashed atomic.Bool

// Crashed reports whether keploy recovered a panic during this run, on any
// goroutine (Recover): a run that crashed failed, whatever code the command
// it stopped left (RunExitCode).
func Crashed() bool { return crashed.Load() }

// RunExitCode returns the code a run exits with when its command left code:
// ExitKeployError if keploy recovered a panic during the run (Crashed) and
// code is 0, else code. Recover stops a crashed run's command, and a command
// that takes that stop for a normal end leaves 0, so a crash used to pass.
// Every binary's main ends its run's log (EndRunLog) and exits with it.
func RunExitCode(code int) int {
	if code == 0 && Crashed() {
		return ExitKeployError
	}
	return code
}

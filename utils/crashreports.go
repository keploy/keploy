package utils

import (
	"sync"

	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

// Crash reports are telemetry. A report carries this run's whole log
// (HandleRecovery attaches it) and names the machine it came from (Sentry's
// server_name is the hostname), so one sent from a run whose user turned
// telemetry off gave that log away. The client used to start in main, before
// the flags and keploy.yml were read, and nothing asked about the opt-out
// after: `keploy record --disable-tele`, or disableTele: true in keploy.yml,
// still sent every crash, from the CLI and from the agent.
//
// Now main only arms it with the build's DSN (ArmCrashReports). The client
// starts once the command has resolved DisableTele (flag over keploy.yml over
// the default, and --enable-testing), and only with telemetry on
// (ResolveCrashReports, from CmdConfigurator.ValidateFlags, which the CLI's
// commands and the agent both run). Until then no client exists and a panic
// sends nothing. A command that never resolves it, one that reads no
// keploy.yml (config, update, login, ...), sends no crash report.
// HandleRecovery reads no log for a report when no client exists.
var crashReports struct {
	mu  sync.Mutex
	dsn string
}

// ArmCrashReports keeps dsn, the build's Sentry DSN, for the crash-report
// client ResolveCrashReports may start. It starts nothing: an armed process
// sends no crash report until its command has resolved whether telemetry is
// on. An empty dsn (a build without one) disarms it.
func ArmCrashReports(dsn string) {
	crashReports.mu.Lock()
	defer crashReports.mu.Unlock()
	crashReports.dsn = dsn
}

// ResolveCrashReports starts the crash-report client when telemetry is on and
// main armed a DSN. With telemetry off it unbinds any client from this
// process, whoever started one, so a panic from here on reports nothing. (A
// client this package starts exists only with telemetry on, so it has nothing
// queued from before the answer.) Called again with the same answer, it
// changes nothing.
func ResolveCrashReports(logger *zap.Logger, disableTele bool) {
	crashReports.mu.Lock()
	defer crashReports.mu.Unlock()
	hub := sentry.CurrentHub()
	if disableTele {
		hub.BindClient(nil)
		return
	}
	if crashReports.dsn == "" || hub.Client() != nil {
		return
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn: crashReports.dsn,
		// Named here: left empty, the SDK works the release out for itself,
		// and its last resort is `git describe` in the working directory --
		// the user's repository, whose .git/config can name a program for git
		// to run (core.fsmonitor). keploy started in a repository then ran
		// that program, and reported the user's commit as its release.
		Release:          crashReportRelease(),
		TracesSampleRate: 1.0,
	})
	if err != nil && logger != nil {
		logger.Debug("Could not initialise sentry.", zap.Error(err))
	}
}

// crashReportRelease is the release a crash report names: this build's
// version, as main set it, and DevVersion when nothing set one.
func crashReportRelease() string {
	if Version == "" {
		return DevVersion
	}
	return Version
}

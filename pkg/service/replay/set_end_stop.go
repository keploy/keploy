package replay

import (
	"context"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// setEndDrainBudget bounds the read of what the app consumed as the set's
// end stopped it (P101-b): the agent's client has no deadline of its own, so
// a read that never ends would hold the run for good. A read that runs out
// leaves the set not judged.
var setEndDrainBudget = 30 * time.Second

// whyAppNotStopped says why keploy read a set's end with its application
// still running, under the proven switch: the set's report carries it
// (models.TestReport.AppNotStopped), as a call the app makes after that read
// is not judged (P101-c).
func whyAppNotStopped(cmdType utils.CmdType, serveTest bool) string {
	switch {
	case cmdType == utils.DockerCompose && serveTest:
		return "keploy keeps the application's docker compose stack running across test-sets and did not stop the application before reading the set's end, so a call it makes after that read is not judged"
	case cmdType == utils.DockerCompose:
		return "the application runs in a docker compose stack whose stop takes keploy's agent down with it, so keploy did not stop the application before reading the set's end, and a call it makes after that read is not judged"
	case serveTest:
		return "--keep-app-alive keeps the application running across test-sets, so keploy did not stop it before reading the set's end, and a call it makes after that read is not judged; it is stopped before a read only at the run's last set, on an attempt no other follows"
	default:
		return "keploy did not start the application (no -c), so it could not stop it before reading the set's end, and a call it makes after that read is not judged"
	}
}

// keepAliveStopper gives the app --keep-app-alive keeps across a run's sets a
// context of its own, under the proven switch, and the stop of it the last
// set's end calls before its read (P101-c): stop cancels the app's context
// with models.ErrAppStoppedForSetEnd and waits until ran has been told how
// its run ended, then says why its stop had to kill it ("" when it did not,
// or when the run's ctx ended first). Not under docker compose, where
// stopping the app stops keploy's agent: stop is nil there, and with the
// switch off, and appCtx is ctx.
func keepAliveStopper(ctx context.Context, cmdType utils.CmdType) (appCtx context.Context, stop func() string, ran func(models.AppError)) {
	if !provenswitch.On() || cmdType == utils.DockerCompose {
		return ctx, nil, func(models.AppError) {}
	}
	appCtx, cancel := context.WithCancelCause(ctx)
	exited := make(chan struct{})
	var killed string
	stop = func() string {
		cancel(models.ErrAppStoppedForSetEnd)
		select {
		case <-exited:
			return killed
		case <-ctx.Done():
			return ""
		}
	}
	ran = func(appErr models.AppError) {
		killed = appErr.StopKilled
		close(exited)
	}
	return appCtx, stop, ran
}

// noAttemptFollows reports whether attempt is the last a set can be given:
// the last flaky check, and no --must-pass, which runs a set again after its
// failing tests are taken out, however many attempts it has had.
func noAttemptFollows(attempt int, test config.Test) bool {
	return attempt == int(test.MaxFlakyChecks) && !test.MustPass
}

// composeAppStopper is an instrumentation that stops and starts the app's
// container of a docker compose stack, its stack running on, under the proven
// switch (AgentClient; P101-c).
type composeAppStopper interface {
	StopComposeApp(ctx context.Context) (string, error)
	// StartComposeApp says whether it started a container that was not
	// running (anew).
	StartComposeApp(ctx context.Context) (anew bool, err error)
}

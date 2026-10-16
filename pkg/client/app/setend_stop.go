package app

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A STOP AT A TEST SET'S END SAYS WHETHER IT HAD TO KILL THE APP.
//
// Under the proven switch keploy stops the app it started for a test set
// before it reads what the app did after the set's last test (P101-b), so a
// call the app makes as it stops is judged with the rest. That stop is the
// teardown's own, the one keploy record stops the app with too: an interrupt,
// then a grace (utils.TreeInterruptGrace for each of a native app's process
// groups, graceBudget for a docker run app), then a kill. Record and replay
// so give a shutdown the same grace. A kill cuts the shutdown short, and what
// the app would have done as it stopped is then not known, so the stop says
// when it had to kill (AppError.StopKilled), and the set's end is not judged,
// naming the stop and its grace.
//
// Two gaps remain, both pre-flip: the stop waits for each of a native app's
// process groups by its leader, so a group member that outlives its leader (a
// pipeline under a shell that died on the interrupt) is not waited for
// (P101-b-m); and a kill the tree interrupt could not make (it could not read
// the process table) or that stopAdopted makes of the app's daemons is not
// said (P101-b-n).
//
// The grace is still a clock. A shutdown near it is killed on a loaded host
// and not on an idle one, so its set can be judged in one run and not in the
// next; and a recording whose stop killed the app lacks the calls the kill
// cut, which a replay that finishes its shutdown then makes, unanswered. A
// stop for both that waits on the app's exit, and a recording that says its
// stop killed, are pre-flip (P101-b-k).

// stopsForSetEnd reports whether ctx was cancelled to stop the app at its
// test set's end (models.ErrAppStoppedForSetEnd).
func stopsForSetEnd(ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(context.Cause(ctx), models.ErrAppStoppedForSetEnd)
}

// noteSetEndKill records why the stop at a set's end killed the app.
func (a *App) noteSetEndKill(why string) {
	a.setEndKill.Store(&why)
	a.logger.Warn(why+"; what it would have done as it stopped is not known, so its test set is not judged (KEPLOY_PROVEN=1)",
		zap.String("app", a.container))
}

// takeSetEndKill is why the latest stop at a set's end had to kill the app,
// once: "" when it did not.
func (a *App) takeSetEndKill() string {
	if why := a.setEndKill.Swap(nil); why != nil {
		return *why
	}
	return ""
}

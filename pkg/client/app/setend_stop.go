package app

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
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
// The stop waits for each of a native app's process groups by its leader. A
// member that outlives its leader (the app of `app & wait`, whose shell dies
// on the interrupt at once) is adopted under the switch, and the stop of what
// the app left behind (stopAdopted, adopt_linux.go) waits for it while it
// makes progress on its way out. It does not ask again a process that acts on
// the interrupt (TakeStopReached), and says when it had to kill one that made
// no progress, as this stop does (P101-b-m). Two gaps remain, both pre-flip:
// a kill the tree interrupt could not make (it could not read the process
// table) is not said (P101-b-n); and where keploy cannot adopt (no pidfds:
// adoptDaemons says so) such a member is not waited for.
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

// TakeStopReached is what the latest stop of a native app reached with its
// interrupt, under the proven switch (utils.InterruptProcessTreeReached),
// once: nil when no such stop read it since it was last taken. Each of its
// processes was asked to stop by that interrupt, and one that does not
// ignore it is acting on it.
func (a *App) TakeStopReached() []utils.Reached {
	if reached := a.stopReached.Swap(nil); reached != nil {
		return *reached
	}
	return nil
}

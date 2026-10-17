package app

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
// where keploy cannot adopt (no pidfds: adoptDaemons says so) such a member
// is not waited for; and on Windows the stop waits for the tree's root only
// (P101-b-o-2).
//
// The grace is still a clock. A shutdown near it is killed on a loaded host
// and not on an idle one, so its set can be judged in one run and not in the
// next. keploy record's stop says when it had to kill too (noteRecordKill),
// and the recording says so in its test-set's config
// (models.TestSet.RecordStopKilled): a replay does not judge a call its app
// makes after the set's last test, which the cut recording may lack
// (P101-b-k). A stop for both that waits on the app's exit, with no grace
// clock, is pre-flip.

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

// StopsARecording reports whether keploy's stop of the app ends a recording
// under the proven switch: such a stop says when it had to kill the app, as
// the stop at a test set's end does (P101-b-k).
func (a *App) StopsARecording() bool {
	return provenswitch.On() && a.opts.Mode == models.MODE_RECORD
}

// noteStopKill records why keploy's stop of the app had to kill it (killed,
// "" when it did not), for the stop at a set's end or at a recording's end;
// another stop's kill is not said.
func (a *App) noteStopKill(setEnd, recordEnd bool, killed string) {
	switch {
	case killed == "":
	case setEnd:
		a.noteSetEndKill("keploy interrupted the application at the set's end and " + killed)
	case recordEnd:
		a.noteRecordKill("keploy stopped the application at the recording's end and " + killed)
	}
}

// noteRecordKill records why keploy record's stop of the app had to kill it.
func (a *App) noteRecordKill(why string) {
	a.setEndKill.Store(&why)
	a.logger.Warn(why+"; what it would have done as it stopped is not in the recording, so a replay of this test-set cannot judge a call its application makes after the set's last test ("+provenswitch.Env+"=1)",
		zap.String("app", a.container),
		zap.String("next_step", "make the application exit promptly on SIGINT, then record the test-set again"))
}

// takeSetEndKill is why the latest stop at a set's end, or at a recording's
// end (noteRecordKill), had to kill the app, once: "" when it did not.
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

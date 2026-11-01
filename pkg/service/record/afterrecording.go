package record

import (
	"context"
	"errors"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// ErrHookStoppedNow is the cause the end-of-recording hook's context ends
// with when the user stops the pass at the interrupt it offered to stop at
// (RunAfterRecordingHook).
var ErrHookStoppedNow = errors.New("the user stopped the post-record pass at a second interrupt")

// RunAfterRecordingHook runs hooks.AfterRecordingComplete for info, and
// returns what it returns. keploy record's stop and keploy mock record run
// the hook through it.
//
// The hook's context keeps ctx's values, but neither its cancellation nor its
// deadline, and has no deadline of its own: the recorder's context is
// already cancelled on the normal stop, and the pass must still run, in
// full. No clock bounds it, so a loaded machine runs the same pass an idle
// one does; a hook bounds its own work by what it counts. The enterprise
// Basic-Auth re-key was cut short by a 30 s deadline here on a loaded
// recorder, and every login it had not re-keyed failed (401) at replay.
//
// So the stop waits for the pass as it waits for its other work that has no
// fixed bound: every `every` while the hook runs, it says it is still
// running and, once the run has been interrupted, offers that another
// interrupt stops it now (utils.StopNowNotice). That interrupt ends the
// hook's context, with ErrHookStoppedNow as its cause, and the hook is
// waited for as it returns: it says what it leaves undone.
func RunAfterRecordingHook(ctx context.Context, logger *zap.Logger, hooks RecordHooks, info *RecordingCompleteContext, every time.Duration) error {
	hookCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	stop := offerToStopTheHook(logger, info.TestSetID, every, cancel)
	defer stop()
	return hooks.AfterRecordingComplete(hookCtx, info)
}

// offerToStopTheHook gives RunAfterRecordingHook's notices, every `every`
// until stop is called: the first says the pass is still running, and the
// one that makes the offer (utils.StopNowNotice) says that another interrupt
// stops it now. That interrupt calls cancel with ErrHookStoppedNow. stop
// returns once the notices are over and the offer is withdrawn.
func offerToStopTheHook(logger *zap.Logger, testSetID string, every time.Duration, cancel context.CancelCauseFunc) (stop func()) {
	if logger == nil {
		logger = zap.NewNop()
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	notice := utils.NewStopNowNotice(every)
	go func() {
		defer close(exited)
		defer notice.Withdraw()
		t := time.NewTicker(every)
		defer t.Stop()
		said, offering := false, false
		for {
			select {
			case <-done:
				return
			case <-notice.Interrupt():
				logger.Warn("Stopping the post-record pass now, at the second interrupt: the recording is kept, without what the pass had left to do",
					zap.String("testSetID", testSetID))
				cancel(ErrHookStoppedNow)
				return
			case <-t.C:
			}
			wasOffering := offering
			offering = notice.Next()
			if !said || offering != wasOffering {
				msg := "Still running the post-record pass over the saved recording"
				if offering {
					msg += "; press Ctrl+C again to stop it now: the recording is then kept without what the pass has left to do"
				}
				logger.Info(msg, zap.String("testSetID", testSetID))
				said = true
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

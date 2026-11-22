package http

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/platform/engine"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// engineWaitSay is how long of keploy's runnable time a docker agent run's
// wait on the container engine (engineWait) goes with no movement of the
// agent's teardown before it says, once, at INFO, what keploy waits for. A
// variable for tests.
var engineWaitSay = 5 * time.Second

// engineWait is a docker-mode agent run's wait on the container engine at its
// end: from the stop's first question to the engine (dockerAgentCancel) until
// the run's own volumes have been asked to go (startAgent). The engine says
// that the agent's container has exited (`wait`), has been removed, and that
// the run's volumes are gone only as fast as its daemon answers, and a daemon
// on a starved host can take longer than the drains' bound on a teardown that
// has stopped moving (30 s). The drains then gave up first, keploy exited, and
// the run's volumes were never removed: they piled up on the host.
//
// So while it runs it holds the drains (utils.BeginAgentStop), as a stop that
// ends on its own does: it is bounded. It ends once the run's volumes have
// been asked to go (end), or once daemonAnswerStall of keploy's runnable time
// has passed with no movement of the agent's teardown (the run's teardown
// scope, which the stop's reports move: stopWatch.moved). A teardown the agent
// still reports moving is its stop's, however long it takes; the bound counts
// from its last movement. keploy cannot tell a daemon that has not said the
// container exited from a container that has not exited, so it waits that one
// bound for either. Its context ends at the bound, with errDaemonSilent, and
// every question the stop and the run's end ask the engine ends with it
// (withinEngineWait): the stop's signals, its wait for the container's exit,
// the wait for its removal and the volumes' removal. A daemon that never
// answers holds keploy's exit for at most the bound, and what it left is said
// at WARN (kdocker.RemoveRunVolumes names each volume and how to remove it).
//
// The zero value is ready to use; its methods may be called from any
// goroutine.
type engineWait struct {
	init  sync.Once
	began sync.Once
	// on is set once it has begun.
	on   atomic.Bool
	ctx  context.Context
	stop context.CancelCauseFunc
	// ended is closed once its hold on the drains is released (begun waits).
	ended chan struct{}
}

func (w *engineWait) context() context.Context {
	w.init.Do(func() {
		w.ctx, w.stop = context.WithCancelCause(context.Background())
		w.ended = make(chan struct{})
	})
	return w.ctx
}

// begin begins the wait (once; later calls only return its context): it holds
// the drains from now until end, or until daemonAnswerStall of keploy's
// runnable time has passed with no movement of moved (nil: none counted), and
// says at INFO, once its stall has passed engineWaitSay, what keploy waits
// for. name is the agent's container. It returns the wait's context, which
// ends with errDaemonSilent at the bound and with context.Canceled at end.
func (w *engineWait) begin(logger *zap.Logger, name string, moved *utils.TeardownScope) context.Context {
	ctx := w.context()
	w.began.Do(func() {
		w.on.Store(true)
		release := utils.BeginAgentStop()
		m := stall.NewMeter(boundClock, daemonAnswerStall)
		go func() {
			defer close(w.ended)
			defer release()
			defer m.Stop()
			said := false
			last := moved.Moves()
			for {
				if now := moved.Moves(); now != last {
					last = now
					m.Reset()
				}
				if ctx.Err() != nil {
					return
				}
				if m.Expired() {
					logger.Warn("The container engine's daemon has not answered keploy's stop of its agent container within keploy's bound: keploy stops waiting for it, and the container may still run",
						zap.String("container", name), zap.Duration("bound", daemonAnswerStall))
					w.stop(errDaemonSilent)
					return
				}
				if !said && m.Stalled() >= engineWaitSay {
					said = true
					logger.Info("Waiting for "+engine.Active().CLI+" to stop keploy's agent container and remove the run's volumes (up to "+daemonAnswerStall.String()+" without an answer from its daemon)",
						zap.String("container", name))
				}
				select {
				case <-ctx.Done():
					return
				case <-m.Wait():
				}
			}
		}()
	})
	return ctx
}

// begun is the wait's context once it has begun (begin), and nil before.
func (w *engineWait) begun() context.Context {
	if !w.on.Load() {
		return nil
	}
	return w.context()
}

// end ends the wait: its context ends, and its hold on the drains is
// released. Later calls, and a begin after it, do nothing more.
func (w *engineWait) end() {
	w.context()
	w.stop(context.Canceled)
}

// withinEngineWait returns ctx ended also once wait ends, with wait's cause:
// a question to the engine of a stop whose run waits on it (engineWait) ends
// at that wait's bound. A nil wait returns ctx itself.
func withinEngineWait(ctx, wait context.Context) (context.Context, func()) {
	if wait == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	unhook := context.AfterFunc(wait, func() { cancel(context.Cause(wait)) })
	return ctx, func() {
		unhook()
		cancel(context.Canceled)
	}
}

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
	init       sync.Once
	began      sync.Once
	silentOnce sync.Once
	// on is set once it has begun.
	on   atomic.Bool
	ctx  context.Context
	stop context.CancelCauseFunc
	// ended is closed once its hold on the drains is released.
	ended chan struct{}

	// mu guards meter state shared between begin's goroutine and
	// expireIfStalled. Never held while calling w.stop or logger.
	mu      sync.Mutex
	m       *stall.Meter
	moved   *utils.TeardownScope
	last    uint64
	stopped bool
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
		w.mu.Lock()
		w.m = stall.NewMeter(boundClock, daemonAnswerStall)
		w.moved = moved
		w.last = moved.Moves()
		w.mu.Unlock()
		go func() {
			defer close(w.ended)
			defer release()
			defer func() {
				w.mu.Lock()
				w.stopped = true
				w.m.Stop()
				w.mu.Unlock()
			}()
			said := false
			for {
				if ctx.Err() != nil {
					return
				}
				expired, stalled := w.checkMeter()
				if expired {
					w.expireSilent(logger, name)
					return
				}
				if !said && stalled >= engineWaitSay {
					said = true
					logger.Info("Waiting for "+engine.Active().CLI+" to stop keploy's agent container and remove the run's volumes (up to "+daemonAnswerStall.String()+" without an answer from its daemon)",
						zap.String("container", name))
				}
				waitCh, expired := w.waitMeter()
				if expired {
					w.expireSilent(logger, name)
					return
				}
				if waitCh == nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-waitCh:
				}
			}
		}()
	})
	return ctx
}

// checkMeter updates movement on w.m under w.mu and reports whether the meter
// is expired and its current stall duration without arming the next wait timer.
func (w *engineWait) checkMeter() (expired bool, stalled time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil || w.stopped {
		return false, 0
	}
	if now := w.moved.Moves(); now != w.last {
		w.last = now
		w.m.Reset()
	}
	if w.m.Expired() {
		return true, 0
	}
	return false, w.m.Stalled()
}

// waitMeter re-checks movement and expiration under w.mu and arms the meter's
// next timer. Calling it after logging keeps the clock's pending timer as the
// barrier after any INFO log for that check.
func (w *engineWait) waitMeter() (waitCh <-chan time.Time, expired bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil || w.stopped {
		return nil, false
	}
	if now := w.moved.Moves(); now != w.last {
		w.last = now
		w.m.Reset()
	}
	if w.m.Expired() {
		return nil, true
	}
	return w.m.Wait(), false
}

// begun is the wait's context once it has begun (begin), and nil before.
func (w *engineWait) begun() context.Context {
	if !w.on.Load() {
		return nil
	}
	return w.context()
}

// expireIfStalled ends the wait synchronously with errDaemonSilent when its
// own movement-aware stall meter has already reached daemonAnswerStall. If
// teardown movement reset the meter or the bound has not yet elapsed, this is
// a no-op so an independent per-question timeout cannot cut short a moving
// teardown.
func (w *engineWait) expireIfStalled(logger *zap.Logger, name string) {
	w.mu.Lock()
	expired := false
	if w.m != nil && !w.stopped {
		if now := w.moved.Moves(); now != w.last {
			w.last = now
			w.m.Reset()
		}
		expired = w.m.Expired()
	}
	w.mu.Unlock()
	if expired {
		w.expireSilent(logger, name)
	}
}

// expireSilent ends the wait with errDaemonSilent and logs the WARN once,
// unless the wait has already ended.
func (w *engineWait) expireSilent(logger *zap.Logger, name string) {
	ctx := w.context()
	w.silentOnce.Do(func() {
		if ctx.Err() != nil {
			return
		}
		logger.Warn("The container engine's daemon has not answered keploy's stop of its agent container within keploy's bound: keploy stops waiting for it, and the container may still run. Remove it yourself once the daemon answers: "+engine.Active().CLI+" rm -f "+name,
			zap.String("container", name), zap.Duration("bound", daemonAnswerStall))
		w.stop(errDaemonSilent)
	})
}

// end ends the wait: its context ends, and its hold on the drains is
// released. Later calls, and a begin after it, do nothing more.
func (w *engineWait) end() {
	w.context()
	w.stop(context.Canceled)
}

// withinEngineWait returns a context of ctx, and the func that ends it: a
// question to the engine of a stop whose run waits on it (engineWait) ends
// at that wait's bound. It ends also once wait ends, with wait's cause; a nil
// wait (a stop no run's wait holds: compose's, keploy's own) adds nothing.
// The func ends it in every case: what a stop runs on it (the resend of its
// SIGTERM) ends with the stop.
func withinEngineWait(ctx, wait context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	unhook := func() bool { return false }
	if wait != nil {
		if wait.Err() != nil {
			cancel(context.Cause(wait))
		} else {
			unhook = context.AfterFunc(wait, func() { cancel(context.Cause(wait)) })
		}
	}
	return ctx, func() {
		unhook()
		cancel(context.Canceled)
	}
}

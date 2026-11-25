package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// engineWaitContainer is the agent container these rows' waits name.
const engineWaitContainer = "keploy-v3-0a1b2c3d4e5f6073"

// engineWaitClock drives boundClock for one test, and returns a func that
// waits until w's look has asked for its next timer, or w has ended: what an
// Advance after it moves is seen by w's next look.
func engineWaitClock(t *testing.T) (*stall.ManualClock, func(w *engineWait)) {
	t.Helper()
	clk := &stall.ManualClock{}
	boundClock = clk
	t.Cleanup(func() { boundClock = nil })
	return clk, func(w *engineWait) {
		t.Helper()
		within(t, "the engine wait's next look", func() {
			for clk.Pending() == 0 {
				select {
				case <-w.ended:
					return
				default:
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

// The run's wait on the engine counts its bound from the agent's teardown's
// last movement, not from its start: a teardown the agent still reports
// moving is its stop's, however long it takes. Past engineWaitSay without a
// movement it says once, at INFO, what keploy waits for and for how long at
// most; at the bound it ends with errDaemonSilent, says so at WARN, and
// releases its hold on the drains.
func TestEngineWaitEndsAtItsBoundSinceTheTeardownLastMoved(t *testing.T) {
	clk, looked := engineWaitClock(t)
	core, logs := observer.New(zapcore.InfoLevel)
	_, scope := utils.NewTeardownScope(context.Background())
	var w engineWait
	if w.begun() != nil {
		t.Fatal("the wait is begun before begin")
	}
	ctx := w.begin(zap.New(core), engineWaitContainer, scope)
	t.Cleanup(w.end)
	if w.begun() != ctx || w.begin(zap.NewNop(), engineWaitContainer, scope) != ctx {
		t.Fatal("begin, again, or begun does not give the wait's one context")
	}
	looked(&w)
	clk.Advance(50 * time.Second)
	looked(&w)
	if said := logs.FilterLevelExact(zapcore.InfoLevel).All(); len(said) != 1 ||
		!strings.Contains(said[0].Message, "to stop keploy's agent container") || !strings.Contains(said[0].Message, daemonAnswerStall.String()) {
		t.Fatalf("past %v the wait said at INFO %v, want once what it waits for, and its bound", engineWaitSay, said)
	}
	// The agent's teardown moves: the bound counts from here.
	scope.Note()
	clk.Advance(time.Second)
	looked(&w)
	clk.Advance(daemonAnswerStall - 5*time.Second)
	looked(&w)
	if ctx.Err() != nil {
		t.Fatalf("the wait ended %v after the teardown last moved, under its bound %v: %v", daemonAnswerStall-5*time.Second, daemonAnswerStall, context.Cause(ctx))
	}
	clk.Advance(5 * time.Second)
	within(t, "the wait's end at its bound", func() { <-w.ended })
	if cause := context.Cause(ctx); !errors.Is(cause, errDaemonSilent) {
		t.Fatalf("the wait ended with %v, want %v", cause, errDaemonSilent)
	}
	if warned := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warned) != 1 || warned[0].ContextMap()["container"] != engineWaitContainer {
		t.Fatalf("at its bound the wait said at WARN %v, want once, naming the container", warned)
	}
	if n := len(logs.FilterLevelExact(zapcore.InfoLevel).All()); n != 1 {
		t.Fatalf("the wait said at INFO %d times, want once", n)
	}
}

// A wait ended (end: the run's volumes have been asked to go) releases its
// hold on the drains at once, its context ended with context.Canceled, and
// says nothing; a begin after it holds nothing.
func TestEngineWaitEndReleasesItsHold(t *testing.T) {
	_, looked := engineWaitClock(t)
	core, logs := observer.New(zapcore.InfoLevel)
	var w engineWait
	ctx := w.begin(zap.New(core), engineWaitContainer, nil)
	looked(&w)
	w.end()
	within(t, "the hold's release at end", func() { <-w.ended })
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		t.Fatalf("the ended wait's cause is %v, want %v", cause, context.Canceled)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("an ended wait said %d lines: %v", n, logs.All())
	}
	var after engineWait
	after.end()
	_ = after.begin(zap.NewNop(), engineWaitContainer, nil)
	within(t, "a begin after end", func() { <-after.ended })
}

// A question within the run's wait on the engine ends with the wait, with its
// cause, and with its own end; a stop no run's wait holds (a nil wait, as
// compose's) still ends what it runs on it with its own end: the resend of its
// SIGTERM runs on that context, and must end with the stop.
func TestWithinEngineWaitEndsWithTheWaitAndWithItsOwnEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait bool
	}{{"no wait", false}, {"a wait", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var wait context.Context
			if tc.wait {
				wait = context.Background()
			}
			ctx, end := withinEngineWait(context.Background(), wait)
			if ctx.Err() != nil {
				t.Fatalf("ended at once: %v", context.Cause(ctx))
			}
			end()
			if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
				t.Fatalf("after its end, its cause is %v, want %v", cause, context.Canceled)
			}
		})
	}
	wait, stop := context.WithCancelCause(context.Background())
	ctx, end := withinEngineWait(context.Background(), wait)
	defer end()
	stop(errDaemonSilent)
	within(t, "the question's end with its wait", func() { <-ctx.Done() })
	if cause := context.Cause(ctx); !errors.Is(cause, errDaemonSilent) {
		t.Fatalf("ended with its wait with %v, want %v", cause, errDaemonSilent)
	}
}

package record

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type panicRecordHooks struct{ BaseRecordHooks }

func (panicRecordHooks) AfterRecordingComplete(context.Context, *RecordingCompleteContext) error {
	panic("hook boom")
}

// Issue #1867 (review R3-2): the end-of-recording hook runs data-driven work
// (the enterprise Basic-Auth re-key). A panic in it must NOT crash the recorder:
// the recording is already saved and the deferred orphan-revoke runs right after
// this call. "Best-effort" has to cover panics, not just returned errors.
func TestAfterRecordingComplete_RecoversFromHookPanic(t *testing.T) {
	r := &Recorder{
		logger: zap.NewNop(),
		config: &config.Config{Path: t.TempDir()},
		hooks:  panicRecordHooks{},
	}
	// Must return normally rather than propagate the panic.
	r.afterRecordingComplete(context.Background(), "test-set-1")
}

type ctxCaptureHooks struct {
	BaseRecordHooks
	gotErr error
}

func (h *ctxCaptureHooks) AfterRecordingComplete(ctx context.Context, _ *RecordingCompleteContext) error {
	h.gotErr = ctx.Err()
	return nil
}

// Issue #1867 (review R3-1): the recorder context is already cancelled on the
// normal SIGINT stop of an interactive recording. Handing it to the hook forces
// the consumer to ignore it (a footgun). afterRecordingComplete must pass a LIVE
// context so the post-record pass isn't skipped and the consumer can safely honor
// its own cancellation/deadline.
func TestAfterRecordingComplete_PassesLiveContext(t *testing.T) {
	h := &ctxCaptureHooks{}
	r := &Recorder{
		logger: zap.NewNop(),
		config: &config.Config{Path: t.TempDir()},
		hooks:  h,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // recorder ctx already cancelled, as on a normal stop

	r.afterRecordingComplete(ctx, "test-set-1")

	if h.gotErr != nil {
		t.Fatalf("hook must receive a live (non-cancelled) context; got ctx.Err()=%v", h.gotErr)
	}
}

type ctxEndHooks struct {
	BaseRecordHooks
	onCall      func(ctx context.Context)
	hasDeadline bool
	errAtStart  error
	causeAtEnd  error
}

func (h *ctxEndHooks) AfterRecordingComplete(ctx context.Context, _ *RecordingCompleteContext) error {
	_, h.hasDeadline = ctx.Deadline()
	h.errAtStart = ctx.Err()
	if h.onCall != nil {
		h.onCall(ctx)
		h.causeAtEnd = context.Cause(ctx)
	}
	return nil
}

func newHookRecorder(t *testing.T, logger *zap.Logger, h RecordHooks) *Recorder {
	t.Helper()
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	return &Recorder{
		logger:     logger,
		config:     &config.Config{Path: t.TempDir()},
		hooks:      h,
		waitNotice: 20 * time.Millisecond,
	}
}

// The end-of-recording hook runs the enterprise Basic-Auth re-key, which
// bounds itself by the comparisons it counts. A 30 s deadline on its context
// cut that pass short on a loaded recorder, which left logins redacted and
// failed them (401) at replay, though an idle machine finished the same
// pass. The hook's context carries no deadline.
func TestAfterRecordingComplete_HookContextHasNoDeadline(t *testing.T) {
	h := &ctxEndHooks{}
	r := newHookRecorder(t, zap.NewNop(), h)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	r.afterRecordingComplete(ctx, "test-set-1")

	if h.hasDeadline {
		t.Fatal("the end-of-recording hook's context has a deadline; a clock must not decide how much of the pass runs")
	}
	if h.errAtStart != nil {
		t.Fatalf("the hook's context had ended before the hook ran: %v", h.errAtStart)
	}
}

// saidWithin waits until logs hold a line with snippet, and reports whether
// they came to. The bound is a fail-safe for a stop that never says it.
func saidWithin(logs *observer.ObservedLogs, snippet string) bool {
	deadline := time.Now().Add(30 * time.Second) // bounds only a stop that never says it
	for logs.FilterMessageSnippet(snippet).Len() == 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

const (
	hookStillSnippet   = "Still running the post-record pass"
	hookOfferSnippet   = "press Ctrl+C again to stop it now"
	hookStoppedSnippet = "Stopping the post-record pass now"
)

// With no clock to end it, the pass is waited for as the stop's other
// unbounded work is: the stop says it is still running and, once the run was
// interrupted, that another interrupt stops it now. That interrupt ends the
// hook's context, and its cause says why, so the hook can say what it leaves.
func TestAfterRecordingComplete_ASecondInterruptAtTheOfferStopsTheHook(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	h := &ctxEndHooks{onCall: func(ctx context.Context) {
		if !saidWithin(logs, hookOfferSnippet) {
			t.Errorf("the stop never offered to end the post-record pass: %v", logs.All())
			return
		}
		if ctx.Err() != nil {
			t.Errorf("the hook's context ended before the user's second interrupt: %v", context.Cause(ctx))
			return
		}
		if !utils.MarkInterruptedAgain() {
			t.Error("the user's press after the offer did not stop the post-record pass")
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second): // bounds only a press that never ends the hook
			t.Error("the user's press after the offer never ended the hook's context")
		}
	}}
	r := newHookRecorder(t, zap.New(core), h)
	utils.MarkInterrupted() // the stop

	r.afterRecordingComplete(context.Background(), "test-set-1")

	if t.Failed() {
		return
	}
	if !errors.Is(h.causeAtEnd, ErrHookStoppedNow) {
		t.Fatalf("the hook's context ended with cause %v, want %v", h.causeAtEnd, ErrHookStoppedNow)
	}
	if logs.FilterMessageSnippet(hookStillSnippet).Len() == 0 {
		t.Fatalf("the stop never said it waits for the post-record pass: %v", logs.All())
	}
	if logs.FilterMessageSnippet(hookStoppedSnippet).FilterField(zap.String("testSetID", "test-set-1")).Len() != 1 {
		t.Fatalf("the stop did not say once that the second interrupt stopped the pass: %v", logs.All())
	}
	if utils.MarkInterruptedAgain() {
		t.Fatal("the offer to stop the pass still stands after the pass ended")
	}
}

// A pass that ends on its own withdraws the offer as it ends: a later
// interrupt is no answer to a wait that is over.
func TestAfterRecordingComplete_ThePassWithdrawsItsOfferAsItEnds(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	h := &ctxEndHooks{onCall: func(context.Context) {
		if !saidWithin(logs, hookOfferSnippet) {
			t.Errorf("the stop never offered to end the post-record pass: %v", logs.All())
		}
	}}
	r := newHookRecorder(t, zap.New(core), h)
	utils.MarkInterrupted() // the stop

	r.afterRecordingComplete(context.Background(), "test-set-1")

	if utils.MarkInterruptedAgain() {
		t.Fatal("the offer to stop the pass still stands after the pass ended")
	}
	if h.causeAtEnd != nil {
		t.Fatalf("the pass's context ended though no one stopped it: %v", h.causeAtEnd)
	}
}

// A stop that began on its own (the app exited, a record timer) has no
// interrupt to answer: the stop still says it waits for the pass, and offers
// nothing until the user interrupts.
func TestAfterRecordingComplete_SaysItWaitsForThePassWithoutAnInterrupt(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	h := &ctxEndHooks{onCall: func(context.Context) {
		if !saidWithin(logs, hookStillSnippet) {
			t.Errorf("the stop never said it waits for the post-record pass: %v", logs.All())
		}
	}}
	r := newHookRecorder(t, zap.New(core), h)

	r.afterRecordingComplete(context.Background(), "test-set-1")

	if n := logs.FilterMessageSnippet(hookOfferSnippet).Len(); n != 0 {
		t.Fatalf("the stop offered to end the pass %d times with no interrupt to answer: %v", n, logs.All())
	}
	if h.causeAtEnd != nil {
		t.Fatalf("the pass's context ended though no one stopped it: %v", h.causeAtEnd)
	}
}

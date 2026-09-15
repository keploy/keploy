package record

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
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

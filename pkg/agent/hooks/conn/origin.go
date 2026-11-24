package conn

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// windowClockCtxKey carries, through the capture context, the clock the
// captured exchange's window start was stamped on: the id of
// the clock (fakeconn.ClockID) of the stamp an ingress took before it
// forwarded the request, which opened the test's window. An ingress that
// stamps the start on a clock no producer names, or on a fallback, sets none.
type windowClockCtxKey struct{}

// WithWindowClock returns a context carrying the clock id of the captured
// exchange's window start: what the ingress stamped its request on before it
// forwarded it. An empty id carries nothing: the start is untrusted.
func WithWindowClock(ctx context.Context, clock string) context.Context {
	if clock == "" {
		return ctx
	}
	return context.WithValue(ctx, windowClockCtxKey{}, clock)
}

// WindowClockFromContext is the clock id WithWindowClock carried on ctx; ""
// when none was set.
func WindowClockFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	clock, _ := ctx.Value(windowClockCtxKey{}).(string)
	return clock
}

// StampOrigin gives tc, under the proven switch, the origin of its window
// start (models.TestCase.Origin): the clock ctx carries (WithWindowClock).
// Nothing without the switch, nor when ctx carries no clock: an untrusted
// start routes nothing. Every capture path that builds a test
// case from an exchange an ingress stamped calls it.
func StampOrigin(ctx context.Context, tc *models.TestCase) {
	if tc == nil || !provenswitch.On() {
		return
	}
	if clock := WindowClockFromContext(ctx); clock != "" {
		tc.Origin = &models.Origin{Clock: clock}
	}
}

// arrivalCtxKey carries, through the capture context, when the ingress had
// the captured exchange complete (WithArrival).
type arrivalCtxKey struct{}

// WithArrival returns a context carrying when the ingress had the captured
// exchange complete, read from the process's clock as it found the
// response's end: where the window of a test case whose response is dated
// before its request ends (models.InvertedWindowEnd). A capture that runs
// later, as a busy agent's do, then takes no call made after that into the
// window. A zero time carries nothing.
func WithArrival(ctx context.Context, at time.Time) context.Context {
	if at.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, arrivalCtxKey{}, at)
}

// ArrivalOf is when the exchange ctx carries was had complete (WithArrival),
// or now, as the capture takes it, when no ingress said. A capture hook of
// its own (CaptureHook) gives it to EndWindow and to its resolve
// (syncMock.ResolveKeptAt), as Capture does.
func ArrivalOf(ctx context.Context) time.Time {
	if ctx != nil {
		if at, ok := ctx.Value(arrivalCtxKey{}).(time.Time); ok {
			return at
		}
	}
	return time.Now()
}

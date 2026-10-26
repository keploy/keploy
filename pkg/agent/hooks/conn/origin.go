package conn

import (
	"context"

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

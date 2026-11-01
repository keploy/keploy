package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A learned read of the application's clock is checked end to end at an
// HTTP call made after another goroutine's calls that keploy served after
// the read (learn.Span.TestFrom): the test's begin moved the app's clock to
// its recorded .933485 (enterprise's run clock, the run told), the handler
// read it after the move and wrote .934 at a cut of a millisecond, and its
// call reached keploy after a consumer's two calls were served (the row's
// times after the move, the read at 0.6 ms and the serves at 1.3 ms and
// 3.5 ms, are illustrative: the logs fix the order only). The call is
// answered; a value read before the test's begin (the previous test's, on
// the moved clock) is refused, naming the field.
func TestStrictChecksAClockReadMadeBeforeAnotherGoroutinesServe(t *testing.T) {
	base := time.Date(2026, 10, 31, 8, 54, 6, 986_000_000, time.UTC)
	recBegin := time.Date(2026, 10, 31, 8, 53, 33, 933_485_077, time.UTC)
	moveTo := base.Add(50 * time.Microsecond)
	off := recBegin.Sub(moveTo)
	clock := func(r time.Time) (time.Time, bool) {
		switch {
		case r.Before(base):
			return r, true
		case r.Before(moveTo):
			return time.Time{}, false
		}
		return r.Add(off), true
	}
	run := func(r time.Time) (time.Time, bool) {
		switch {
		case r.Before(base):
			return time.Time{}, true
		case r.Before(moveTo):
			return time.Time{}, false
		}
		return moveTo, true
	}
	ctx := func() context.Context {
		tr := learn.NewTracker()
		tr.SetAppClock(clock)
		tr.SetAppClockRun(run)
		tr.NewSetAt(base.Add(-time.Second))
		tr.ServedMockAt(httpMock("previous", "GET", "/v1/account"), base.Add(-25*time.Millisecond))
		tr.BeginAt(base.Add(-100 * time.Microsecond))
		tr.ServedMockAt(httpMock("consumer-1", "GET", "/v1/events"), moveTo.Add(1300*time.Microsecond))
		tr.ServedMockAt(httpMock("consumer-2", "POST", "/v1/events"), moveTo.Add(3500*time.Microsecond))
		tr.SetLearnEnded(true)
		return learn.WithTracker(context.Background(), tr)
	}
	stamp := func(at time.Time) string {
		return at.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
	}
	mock := recorded("upsert", "/v1/urls", map[string]string{"Content-Type": "application/json"},
		`{"created":"`+stamp(recBegin)+`","url":"https://google.com"}`)
	mock.Spec.Clocks = map[string]models.ClockOffset{"body.created": {Res: time.Millisecond}}
	call := func(created time.Time) *req {
		r := sent("/v1/urls", map[string]string{"Content-Type": "application/json"},
			`{"created":"`+stamp(created)+`","url":"https://google.com"}`)
		r.arrived = moveTo.Add(4400 * time.Microsecond)
		return r
	}
	read := moveTo.Add(600 * time.Microsecond).Add(off)
	if got, diag := strictMatchIn(t, ctx(), call(read), nil, nil, nil, mock); got == nil {
		t.Fatalf("the handler's read (%s), made before the consumer's serves, refused (%v); want it answered", stamp(read), differs(diag))
	}
	stale := recBegin.Add(-2 * time.Millisecond)
	if got, diag := strictMatchIn(t, ctx(), call(stale), nil, nil, nil, mock); got != nil || !hasField(differs(diag), "body.created") {
		t.Fatalf("a read before the test's begin (%s): answered %v, differs %v; want refused naming body.created", stamp(stale), got != nil, differs(diag))
	}
}

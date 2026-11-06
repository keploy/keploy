package learn

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

var serveClockT0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// serveAt makes c's serves noted at the instants at, in order, by the
// serve clock's clock (serveNow), set back once the test ends.
func serveAt(t *testing.T, c *ServeClock, at ...time.Time) {
	t.Helper()
	prev := serveNow
	t.Cleanup(func() { serveNow = prev })
	for _, a := range at {
		serveNow = func() time.Time { return a }
		c.Served()
	}
}

func realClock(real time.Time) (time.Time, bool) { return real, true }

// A call's span starts at the latest serve noted before its arrival, of any
// flow (a serve noted after it, or at its own instant, starts none), and
// ends at its arrival, on the clock the agent handed.
func TestServeClock_ACallsSpanStartsAtTheLatestServeBeforeIt(t *testing.T) {
	c := &ServeClock{}
	c.SetClock(realClock)
	arrival := serveClockT0.Add(10 * time.Second)
	serveAt(t, c, serveClockT0.Add(2*time.Second), serveClockT0.Add(7*time.Second), arrival, arrival.Add(time.Second))
	s, ok := c.SpanTo(arrival)
	if !ok || !s.From.Equal(serveClockT0.Add(7*time.Second)) || !s.To.Equal(arrival) {
		t.Fatalf("span %v..%v %v; want the serve 7 s in to the arrival", s.From, s.To, ok)
	}
}

// No span without a clock, a serve before the arrival, or with a clock that
// moved between them (it did not run as one), and none from a nil serve
// clock.
func TestServeClock_NoSpanWithoutAClockAServeBeforeOrOneRun(t *testing.T) {
	arrival := serveClockT0.Add(10 * time.Second)
	c := &ServeClock{}
	serveAt(t, c, serveClockT0)
	if s, ok := c.SpanTo(arrival); ok {
		t.Fatalf("no clock handed: span %v..%v", s.From, s.To)
	}
	c.SetClock(realClock)
	if _, ok := c.SpanTo(arrival); !ok {
		t.Fatal("the real clock handed: no span")
	}
	if s, ok := c.SpanTo(serveClockT0); ok {
		t.Fatalf("no serve before the arrival: span %v..%v", s.From, s.To)
	}
	c.SetClock(func(real time.Time) (time.Time, bool) {
		if real.Before(serveClockT0.Add(5 * time.Second)) {
			return real, true
		}
		return real.Add(time.Hour), true
	})
	if s, ok := c.SpanTo(arrival); ok {
		t.Fatalf("a clock that moved between the serve and the arrival: span %v..%v", s.From, s.To)
	}
	c.SetClock(func(time.Time) (time.Time, bool) { return time.Time{}, false })
	if _, ok := c.SpanTo(arrival); ok {
		t.Fatal("a clock the hooks cannot tell: a span")
	}
	c.SetClock(nil)
	if _, ok := c.SpanTo(arrival); ok {
		t.Fatal("the clock taken back: a span")
	}
	var none *ServeClock
	none.Served()
	none.SetClock(realClock)
	if _, ok := none.SpanTo(arrival); ok {
		t.Fatal("a nil serve clock: a span")
	}
}

// The ring keeps the latest serves: once more serves than it keeps landed
// after a call's arrival, the serve before it is gone and the call has no
// span (fail closed), never one from an older serve.
func TestServeClock_ACallWhosePreviousServeLeftTheRingHasNoSpan(t *testing.T) {
	c := &ServeClock{}
	c.SetClock(realClock)
	arrival := serveClockT0.Add(10 * time.Second)
	at := []time.Time{serveClockT0.Add(time.Second), serveClockT0.Add(9 * time.Second)}
	for i := 0; i < serveClockKept-1; i++ {
		at = append(at, arrival.Add(time.Duration(i+1)*time.Millisecond))
	}
	serveAt(t, c, at...)
	if s, ok := c.SpanTo(arrival); !ok || !s.From.Equal(serveClockT0.Add(9*time.Second)) {
		t.Fatalf("the serve before kept: span from %v %v; want 9 s in", s.From, ok)
	}
	serveAt(t, c, arrival.Add(time.Second))
	if s, ok := c.SpanTo(arrival); ok {
		t.Fatalf("the serve before left the ring: span from %v", s.From)
	}
}

// CallSpanOf reads the serve clock on ctx (the switch off), else the learn
// tracker's span of no recording (under the switch), and nothing without
// either.
func TestCallSpanOf_ReadsTheServeClockElseTheTracker(t *testing.T) {
	arrival := serveClockT0.Add(10 * time.Second)
	var noCtx context.Context
	if _, ok := CallSpanOf(noCtx, arrival); ok {
		t.Fatal("a nil ctx: a span")
	}
	if _, ok := CallSpanOf(context.Background(), arrival); ok {
		t.Fatal("neither on ctx: a span")
	}
	c := &ServeClock{}
	c.SetClock(realClock)
	serveAt(t, c, serveClockT0.Add(3*time.Second))
	tr := NewTracker()
	tr.SetAppClock(realClock)
	tr.ServedMockAt(&models.Mock{Name: "m"}, serveClockT0.Add(8*time.Second))

	if s, ok := CallSpanOf(WithServeClock(context.Background(), c), arrival); !ok || !s.From.Equal(serveClockT0.Add(3*time.Second)) {
		t.Fatalf("the serve clock on ctx: span from %v %v; want 3 s in", s.From, ok)
	}
	want, wok := ClockSpanOf(WithTracker(context.Background(), tr), arrival, nil)
	if !wok || !want.From.Equal(serveClockT0.Add(8*time.Second)) {
		t.Fatalf("the tracker's span from %v %v; want 8 s in", want.From, wok)
	}
	if s, ok := CallSpanOf(WithTracker(context.Background(), tr), arrival); !ok || s != want {
		t.Fatalf("the tracker on ctx: span %+v %v; want the tracker's %+v", s, ok, want)
	}
}

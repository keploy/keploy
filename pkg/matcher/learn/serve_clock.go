package learn

import (
	"context"
	"sync"
	"time"
)

// A CALL'S SPAN WITH THE PROVEN SWITCH OFF, FOR THE BACKGROUND CHECK ALONE.
//
// A call no recording answered is a background miss when it is the same
// request as one of the set's background recordings, apart from its reads
// of the application's clock (models.MockMismatchReport.Background). A read
// of the clock holds on the call's span: from the latest call keploy served
// before it to its arrival, on the application's clock. Under the proven
// switch the learn tracker on the parser's ctx tells it (ClockSpanOf). With
// the switch off the tracker is fed no serve and is put on no parser's ctx,
// and that stays so: a ServeClock tells the span instead, from the serves
// the store notes to it, and is read by CallSpanOf alone. Nothing the learn
// step learns reads it.

// serveClockKept is how many serves a ServeClock keeps: a call whose span
// starts before the oldest of them has none (fail closed), so it bounds
// how many serves may land between a call's arrival and its check.
const serveClockKept = 256

// ServeClock keeps the instants of the latest serves keploy made with the
// proven switch off, and the application's clock to read them on
// (SetClock). The zero value is ready; a nil one tells no span.
type ServeClock struct {
	mu    sync.Mutex
	clock func(real time.Time) (time.Time, bool)
	// served is a ring of serve instants in the order they were noted, which
	// is their time order (Served reads the clock holding mu); next is
	// where the next one is written. A slot not yet written holds the zero
	// instant, which starts no span.
	served [serveClockKept]time.Time
	next   int
}

// serveNow is the clock a ServeClock notes a serve by.
var serveNow = time.Now

// SetClock makes c read the application's clock at a real instant through
// f, as the agent's hooks tell it (false where they cannot); nil for none,
// and then c tells no span.
func (c *ServeClock) SetClock(f func(real time.Time) (time.Time, bool)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.clock = f
	c.mu.Unlock()
}

// Served notes a serve made now: a later call's span starts no earlier.
func (c *ServeClock) Served() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.served[c.next] = serveNow()
	c.next = (c.next + 1) % serveClockKept
	c.mu.Unlock()
}

// SpanTo is the span on the application's clock of a call that reached
// keploy at arrival: from the latest serve noted before arrival, of any
// flow, to arrival, where the application's clock ran as one between them
// (liveClock.from). false with no clock, no serve kept before arrival (the
// ring walked from its latest back stops at the first before arrival: an
// unwritten slot's zero instant starts none), or a clock that moved between
// them.
func (c *ServeClock) SpanTo(arrival time.Time) (Span, bool) {
	if c == nil {
		return Span{}, false
	}
	c.mu.Lock()
	lc := liveClock{clock: c.clock}
	for k := 1; k <= serveClockKept; k++ {
		if at := c.served[(c.next-k+serveClockKept)%serveClockKept]; at.Before(arrival) {
			lc.prev = at
			break
		}
	}
	c.mu.Unlock()
	return lc.span(arrival)
}

type serveClockKey struct{}

// WithServeClock puts c on ctx, for the parsers of a connection with the
// proven switch off (CallSpanOf).
func WithServeClock(ctx context.Context, c *ServeClock) context.Context {
	return context.WithValue(ctx, serveClockKey{}, c)
}

// CallSpanOf is the span on the application's clock of a call that reached
// keploy at arrival, for the check that a call no recording answered is one
// more run of the set's background work, and for nothing else: the serve
// clock's on ctx (WithServeClock, the proven switch off), else the learn
// tracker's, of no recording (ClockSpanOf, under the switch). false where
// neither tells one.
func CallSpanOf(ctx context.Context, arrival time.Time) (Span, bool) {
	if ctx == nil {
		return Span{}, false
	}
	if c, ok := ctx.Value(serveClockKey{}).(*ServeClock); ok {
		return c.SpanTo(arrival)
	}
	return ClockSpanOf(ctx, arrival, nil)
}

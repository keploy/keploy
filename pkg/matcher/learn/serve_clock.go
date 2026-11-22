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
// of the clock holds on one of the call's spans: from the running set's
// begin to its arrival, on the application's clock (CallSpansOf), and, for
// CallSpanOf, from the latest call keploy served before it. Under the
// proven switch the learn tracker on the parser's ctx tells them
// (ClockSpansOf, ClockSpanOf). With the switch off the tracker is fed no
// serve and is put on no parser's ctx, and that stays so: a ServeClock
// tells them instead, from the set's begin and the serves the store notes
// to it, and is read by CallSpansOf and CallSpanOf alone. Nothing the learn
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
	// set is the real instant the running test set began (NewSetAt), zero
	// before the first: a background call's span starts there (SpansOf).
	set time.Time
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

// NewSet notes that a test set begins now (NewSetAt).
func (c *ServeClock) NewSet() { c.NewSetAt(serveNow()) }

// NewSetAt notes that a test set began at the real instant at: a later
// background call's span starts there (SpansOf). The application reads its
// clock for a call of the set after the set began.
func (c *ServeClock) NewSetAt(at time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.set = at
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

// SpansOf is the spans on the application's clock of a background call
// that reached keploy at arrival (CallSpansOf): the one span from the
// running set's begin (NewSetAt) to arrival, where the application's clock
// ran as one between them, never from a serve. A background poller's read
// of its clock is ordered with no call keploy serves, not even a run of
// the same poll: several goroutines may run one poll, sending the same
// request, and keploy cannot tell them apart. Any serve made between the
// read and the arrival (the open test's call, a worker's, another poller's)
// would start the span after the read. false with no clock, before any set
// began, for a call that reached keploy before the running set began, and
// where the clock did not run as one since the set's begin: with the
// switch off the hooks tell a clock only where nothing moves it
// (agent.BackgroundClockSetter).
func (c *ServeClock) SpansOf(arrival time.Time) ([]Span, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	clock, set := c.clock, c.set
	c.mu.Unlock()
	if clock == nil {
		return nil, false
	}
	s, ok := liveClock{clock: clock}.from(set, arrival)
	if !ok {
		return nil, false
	}
	return []Span{s}, true
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

// CallSpansOf is the spans on the application's clock of a background
// call that reached keploy at arrival, for the same check alone: its read
// of the clock holds in one of them (EpochOffset.HoldsIn). Each is bounded
// by the running set's begin, never by a serve: keploy cannot tell which
// call a background poller's read of the clock is ordered after, and a
// serve made between the read and the arrival would start a span after the
// read (Tracker.BackgroundSpans under the switch, ServeClock.SpansOf with
// it off). Under the switch, where the clock moved after the set's begin,
// they hold what it showed from there to the move that began the run to
// arrival, one span per offset in force, so a value a move jumped over
// holds in none. The serve clock's is one span: with the switch off the
// hooks tell a clock only where nothing moves it. false where neither
// tells one.
func CallSpansOf(ctx context.Context, arrival time.Time) ([]Span, bool) {
	if ctx == nil {
		return nil, false
	}
	if c, ok := ctx.Value(serveClockKey{}).(*ServeClock); ok {
		return c.SpansOf(arrival)
	}
	return ClockSpansOf(ctx, arrival)
}

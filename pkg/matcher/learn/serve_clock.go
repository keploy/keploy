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
	// names are the names of the recordings served, by slot of served
	// ("" where Served noted one of no name): a background call's span
	// starts at its own group's serve (SpansOf).
	names [serveClockKept]string
	next  int
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

// Served notes a serve made now, of a recording of no name: a later call's
// span starts no earlier (SpanTo).
func (c *ServeClock) Served() { c.ServedMock("") }

// ServedMock notes a serve made now of the recording named name: a later
// call's span starts no earlier (SpanTo), and a later background call of
// that recording's group may start its spans there (SpansOf).
func (c *ServeClock) ServedMock(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.served[c.next], c.names[c.next] = serveNow(), name
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

// SpansOf is the spans on the application's clock of a background call
// that reached keploy at arrival, whose own background group's recordings
// ofGroup names (CallSpansOf): the span from the latest serve kept before
// arrival of a recording of the group (the poller's previous run, which
// it read its clock after), else from the earliest serve kept before
// arrival, to arrival, where the application's clock ran as one between
// them, never from another flow's serve alone: a poller's read is ordered
// with no other flow's call, which may be served between the read and the
// arrival. Where the clock did not run as one from that bound (it moved
// since), SpanTo's span, from the latest serve of any flow: with the
// switch off nothing tells what the clock showed before a move. false
// where neither is told, and for a nil ofGroup (no group named).
func (c *ServeClock) SpansOf(arrival time.Time, ofGroup func(name string) bool) ([]Span, bool) {
	if c == nil || ofGroup == nil {
		return nil, false
	}
	c.mu.Lock()
	lc := liveClock{clock: c.clock}
	var latest time.Time
	// Walked from the latest serve back, to the first of the group's before
	// arrival or the first slot not yet written (its zero instant): every
	// serve noted is after the one before it (Served).
	for k := 1; k <= serveClockKept; k++ {
		j := (c.next - k + serveClockKept) % serveClockKept
		at := c.served[j]
		if at.IsZero() {
			break
		}
		if !at.Before(arrival) {
			continue
		}
		if latest.IsZero() {
			latest = at
		}
		lc.prev = at
		if ofGroup(c.names[j]) {
			break
		}
	}
	c.mu.Unlock()
	if s, ok := lc.span(arrival); ok {
		return []Span{s}, true
	}
	lc.prev = latest
	s, ok := lc.span(arrival)
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

// CallSpansOf is the spans on the application's clock of a call that
// reached keploy at arrival, for the same check alone, of a call whose own
// background group's recordings ofGroup names by name (the recordings it
// is one more run of but for its reads of the clock): its read of the
// clock holds in one of them (EpochOffset.HoldsIn). Each is bounded by the
// group's latest serve before arrival, never by another flow's alone
// (Tracker.BackgroundSpans under the switch, ServeClock.SpansOf with it
// off); under the switch, where the clock moved after the bound, they hold
// what it showed from the bound to the move that began the run to arrival.
// The serve clock's is one span: with the switch off the hooks tell a span
// only on a clock nothing moves. false where neither tells one, and for a
// nil ofGroup.
func CallSpansOf(ctx context.Context, arrival time.Time, ofGroup func(name string) bool) ([]Span, bool) {
	if ctx == nil {
		return nil, false
	}
	if c, ok := ctx.Value(serveClockKey{}).(*ServeClock); ok {
		return c.SpansOf(arrival, ofGroup)
	}
	return ClockSpansOf(ctx, arrival, ofGroup)
}

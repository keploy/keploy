package relay

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// watermark.go is proxy mode's capture watermark: what the parsers of the
// connections it records have got through, from their relays' tees
// (progress.go).
//
// A recording asks it two things (syncMock settle.go):
//
//   - whether the verdict for a test case is final (Settled): no span that
//     could overlap its window, and no mock of it, can still come from a
//     parser. A parser runs behind its connection's traffic by up to a full
//     capture buffer, and learns it fails only when it gets there: a test
//     case checked as it completes can be saved without the mocks a parser
//     still had to get to, or without the span its failure opens;
//   - whether the capture may still hand over something from before a time
//     (PendingBefore): what a recording's stop drains the agent's streams
//     until, instead of until they fall quiet. A parser behind the traffic can
//     be quiet for longer than any grace.
//
// It follows the connections whose parser is running; one whose parser has
// returned can open no span from its parsing and hand over no mock (what its
// connection carries after is left out by syncMock.UnrecordedConn).

// Watermark follows the relays of the connections whose parser is running. The
// zero value is not usable; see NewWatermark. Safe for concurrent use.
type Watermark struct {
	mu    sync.Mutex
	conns map[*Relay]string
	// retired is what the connections no longer followed consumed, plus one
	// for each: with what the followed ones consumed, a count that moves
	// whenever a parser makes progress or returns (Progress).
	retired atomic.Uint64
}

// NewWatermark returns a Watermark that follows no connection yet.
func NewWatermark() *Watermark {
	return &Watermark{conns: make(map[*Relay]string)}
}

// Track follows r, whose parser is about to run, named for what holds the
// watermark (its connection and parser), and returns the func that stops
// following it once its parser has returned. The func is idempotent.
func (w *Watermark) Track(r *Relay, name string) (untrack func()) {
	if w == nil || r == nil {
		return func() {}
	}
	w.mu.Lock()
	w.conns[r] = name
	w.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			w.retired.Add(r.consumedBytes() + 1)
			w.mu.Lock()
			delete(w.conns, r)
			w.mu.Unlock()
		})
	}
}

type trackedRelay struct {
	r    *Relay
	name string
}

func (w *Watermark) snapshot() []trackedRelay {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]trackedRelay, 0, len(w.conns))
	for r, name := range w.conns {
		out = append(out, trackedRelay{r, name})
	}
	return out
}

// waitingOn is what holds the watermark: a connection, and where its parser
// is. Formatted only when it is told.
type waitingOn struct {
	name, why string
}

func (b waitingOn) String() string { return b.name + ": " + b.why }

// behind returns the first followed connection that carried bytes within
// [start, end] and whose parser is not done with every byte up to end, nil
// when none is.
func (w *Watermark) behind(start, end time.Time) *waitingOn {
	if w == nil || end.IsZero() {
		return nil
	}
	s, e := int64(0), end.UnixNano()
	if !start.IsZero() {
		s = start.UnixNano()
	}
	for _, c := range w.snapshot() {
		if c.r.carriedNothingIn(s, e) {
			continue
		}
		if ok, why := c.r.passedThrough(e); !ok {
			return &waitingOn{name: c.name, why: why}
		}
	}
	return nil
}

// Settled reports whether the verdict for a test case recorded over
// [start, end] is final as far as the capture goes: every connection that
// carried bytes within its window has a parser done with everything up to its
// end (Relay.passedThrough), so each of its mocks has been handed on and each
// span a parser opens over it has been opened. A test case with no end makes
// no claim, and is settled. namespace and pod are not used: proxy mode records
// one application.
func (w *Watermark) Settled(_, _ string, start, end time.Time) bool {
	return w.behind(start, end) == nil
}

// HeldBy says what keeps the verdict for a test case recorded over
// [start, end] from being final, nil when nothing does (syncMock.Explainer).
func (w *Watermark) HeldBy(_, _ string, start, end time.Time, _ uint64) fmt.Stringer {
	if b := w.behind(start, end); b != nil {
		return b
	}
	return nil
}

// PendingBefore reports whether a parser may still hand over a mock captured
// at or before `before`, or open a span over it: a connection that carried
// bytes by then has a parser not done with them.
func (w *Watermark) PendingBefore(before time.Time) bool {
	return w.behind(time.Time{}, before) != nil
}

// PendingOn says what may still hand over something captured at or before
// `before`, nil when nothing may.
func (w *Watermark) PendingOn(before time.Time) fmt.Stringer {
	if b := w.behind(time.Time{}, before); b != nil {
		return b
	}
	return nil
}

// Progress is a count that moves whenever a followed parser consumes bytes or
// returns: a stop that waits on the capture can tell a parser working through
// its backlog from one that is not moving.
func (w *Watermark) Progress() uint64 {
	if w == nil {
		return 0
	}
	n := w.retired.Load()
	for _, c := range w.snapshot() {
		n += c.r.consumedBytes()
	}
	return n
}

// CapturedTo is how far the capture of every followed connection has got
// (Relay.CapturedTo): the earliest of them; the clock when it follows none. The
// recording's open orphan spans extend no further (syncMock.Horizon).
func (w *Watermark) CapturedTo() time.Time {
	to := time.Now()
	if w == nil {
		return to
	}
	for _, c := range w.snapshot() {
		if t := c.r.CapturedTo(); t.Before(to) {
			to = t
		}
	}
	return to
}

// Following is how many connections the watermark follows.
func (w *Watermark) Following() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.conns)
}

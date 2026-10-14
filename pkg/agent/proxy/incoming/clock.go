package proxy

import (
	"errors"
	"io"
	"sync/atomic"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// ingressClock is a clock the ingress reads its stamps from and waits on, in
// place of the system's (stubbedClock).
type ingressClock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

// stubbedClock, if set, stands in for the system clock in the ingress: the
// stamps the synchronous loop's windows are made of (clockNow, also behind the
// request stamps of the zero-copy loop, which shares wireTimeConn), and the
// waits for the clock to pass them (waitPastYield). A test's way to give the
// ingress a clock as coarse as Windows'.
var stubbedClock atomic.Pointer[ingressClock]

// clockNow is the ingress's time.Now: what its exchanges' windows are stamped
// with (a request's first byte, a response's headers and last byte).
func clockNow() time.Time {
	if c := stubbedClock.Load(); c != nil {
		return c.now()
	}
	return time.Now()
}

func clockSleep(d time.Duration) {
	if c := stubbedClock.Load(); c != nil {
		c.sleep(d)
		return
	}
	time.Sleep(d)
}

const (
	// clockRereads is how many times waitPastYield reads the clock again
	// before it first sleeps: a few microseconds, which a clock of
	// microseconds (macOS's wall clock, gettimeofday's) passes in, where a
	// sleep would cost far more than the wait. A count, not a time: on
	// Windows the monotonic reading moves only at the tick too, and a spin
	// bounded by time would spin the tick out.
	clockRereads = 64
	// clockPoll is how long waitPastYield first sleeps between two readings
	// of the clock, well under a tick of Windows' (0.5 to 15.6 ms). Each
	// sleep doubles it, up to clockPollMax: a wait of a 15.6 ms tick sleeps
	// about 19 times (50 µs steps would wake about 300 times, where the
	// timer honours them), and ends at most clockPollMax past the tick.
	clockPoll    = 50 * time.Microsecond
	clockPollMax = time.Millisecond
	// clockWaitMax bounds waitPastYield, well past the coarsest tick of a
	// clock it may run on (Windows' default, 15.6 ms): past it the wall clock
	// was set back, and no wait makes it pass the yield soon.
	clockWaitMax = 100 * time.Millisecond
)

// waitPastYield returns once the clock reads later than at, the instant win
// stopped claiming alone what is made (syncMock.Window.Yield) or the end of its
// response. The synchronous loop calls it before anything it does after that
// instant can be seen: before it gives the lock back (the next request is
// read, and makes its calls, from then on), before it forwards the headers a
// window yielded at (a client may answer them, and a request that gave the
// lock back at its body of unknown length may already run beside this one),
// and before the client can see a response's end (endWaitBody). A nil window
// claims nothing: it returns at once.
//
// Mocks are attributed by time alone, and a window claims what was made up to
// its yield, and to its end, both included. A call that something the loop
// did caused is ordered after it, but in time only if the clock has moved on
// in between: on a clock coarser than the time from there to the call, the
// call reads the very instant the window yielded or ended at, and that window
// takes it (the right test case is recorded without it, the window's with
// it). Windows' clock is one: time.Now moves on only at the system timer's
// tick, 0.5 to 15.6 ms. Waiting here makes what the loop orders ordered in
// time too, on any clock: every stamp read after it returns, by the loop or by
// a parser stamping a mock, is later than at. Linux's clock reads nanoseconds
// and has moved on already: it returns at once. macOS's wall clock reads
// microseconds, and passes at within the few reads made before a first sleep
// (clockRereads). On Windows it waits out the rest of a tick at most.
//
// What the loop does not cause it does not separate: a call that another
// request still in flight (a stream that gave the lock back at its headers)
// makes on its own, in the tick a window starts or ends in, reads that
// window's start or end, and goes with it. Only stamps that never repeat (one
// clock for every stamp in the process, each later than the last) would order
// that.
//
// Both readings of the clock must pass at: Before and After compare monotonic
// readings when both stamps carry one, the mock manager compares wall readings
// (Unix nanoseconds), and Windows loads the two in separate reads, so a tick
// between them can pass one and not the other. No unit test can tell the two
// apart: a stubbed time carries no monotonic reading, and a real clock's two
// move together. A wall clock set back does not pass at for as long as it was
// set back, and past clockWaitMax this gives up: attribution by time does not
// hold across such a step whatever the loop waits for.
func waitPastYield(win *syncMock.Window, at time.Time) {
	if win == nil || at.IsZero() {
		return
	}
	start, nap := clockNow(), clockPoll
	for n, reads := start, 0; !n.After(at) || n.UnixNano() <= at.UnixNano(); n = clockNow() {
		if n.Sub(start) >= clockWaitMax {
			return
		}
		if reads++; reads <= clockRereads {
			continue
		}
		clockSleep(nap)
		nap = min(2*nap, clockPollMax)
	}
}

// endWaitBody is a response body that runs atEnd, once, before it returns the
// read that ends it: the one that brings it to its length (left, when known),
// or that finds its end (io.EOF). The client can tell a body has ended only
// from that read's bytes (a known length), or from what is written after it
// (the last chunk of a chunked body, the close that ends one of unknown
// length), so atEnd runs before the client can see the end.
//
// For a known length the end is found by counting: net/http's body (Go 1.27)
// returns io.EOF with the last bytes, so the EOF alone finds it today, but a
// body that returned them without it would have them written (the copy stops
// at the length) before any read found the end.
type endWaitBody struct {
	io.ReadCloser
	left  int64 // what is left of a known length; -1 when unknown
	atEnd func()
	ended bool
}

func (b *endWaitBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.left >= 0 {
		b.left -= int64(n)
	}
	if !b.ended && (b.left == 0 || errors.Is(err, io.EOF)) {
		b.ended = true
		b.atEnd()
	}
	return n, err
}

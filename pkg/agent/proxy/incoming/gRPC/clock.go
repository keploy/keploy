package proxy

import (
	"sync/atomic"
	"time"
)

// forwardClock is a clock the forwarder stamps chunks with and waits on, in
// place of the system's (stubbedClock).
type forwardClock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

// stubbedClock, if set, stands in for the system clock in the forwarder: the
// stamps of the chunks it tees (clockNow) and the wait for the clock to pass
// a stream's end (waitPast). A test's way to give it a clock as coarse as
// Windows'.
var stubbedClock atomic.Pointer[forwardClock]

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
	// clockRereads is how many times waitPast reads the clock again before it
	// first sleeps: a few microseconds, which a clock of microseconds passes
	// in, where a sleep would cost far more than the wait. A count, not a time:
	// on Windows the monotonic reading moves only at the tick too.
	clockRereads = 64
	// clockPoll is how long waitPast first sleeps between two readings, well
	// under a tick of Windows' (0.5 to 15.6 ms); each sleep doubles it, up to
	// clockPollMax.
	clockPoll    = 50 * time.Microsecond
	clockPollMax = time.Millisecond
	// clockWaitMax bounds waitPast, well past the coarsest tick of a clock it
	// may run on (Windows' default, 15.6 ms): past it the wall clock was set
	// back, and no wait makes it pass the stamp soon.
	clockWaitMax = 100 * time.Millisecond
)

// waitPast returns once the clock reads later than at, the stamp of a frame
// that ends a stream: the forwarder calls it before the client can see that
// frame's last byte. Mocks are attributed by time alone, and a call's window
// takes what was made up to its end, the end included: on a clock coarser
// than the time from there to the client's next call, that call, and the
// calls the app makes for it, would read the very instant the window ends at,
// and go to it. On Linux the clock reads nanoseconds and has moved on already:
// it returns at once; on Windows it waits out the rest of a tick at most. Both
// readings must pass at: Before and After compare monotonic readings, the
// mock manager compares wall readings (Unix nanoseconds). This is the ingress
// HTTP/1 loop's waitPastYield, for the forwarder.
func waitPast(at time.Time) {
	if at.IsZero() {
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

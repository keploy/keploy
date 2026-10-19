package proxy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// waitPast returns once the forwarder's clock reads later than the stamp, on
// every clock it may run on: at once on the system clock, which has moved on
// already; at the next tick on a clock as coarse as Windows'.
func TestWaitPastReturnsOnceTheClockReadsLaterThanTheStamp(t *testing.T) {
	for _, tick := range []time.Duration{0, windowsTick, time.Millisecond} {
		t.Run(clockName(tick), func(t *testing.T) {
			if tick != 0 {
				stubQuantizedClock(t, tick)
			}
			awaitTickStart(tick)
			at := clockNow()
			waitPast(at)
			n := clockNow()
			assert.True(t, n.After(at), "the clock reads %v after the wait, not past the stamp %v", n, at)
			assert.Greater(t, n.UnixNano(), at.UnixNano(), "the wall reading is not past the stamp")
		})
	}
}

// A stamp the clock may not reach soon (the wall clock was set back) ends the
// wait at its bound, not never; a zero stamp is past already.
func TestWaitPastIsBounded(t *testing.T) {
	back := time.Hour
	prev := stubbedClock.Swap(&forwardClock{now: func() time.Time { return time.Now().Add(-back) }, sleep: time.Sleep})
	t.Cleanup(func() { stubbedClock.Store(prev) })
	start := time.Now()
	waitPast(time.Now())
	took := time.Since(start)
	assert.GreaterOrEqual(t, took, clockWaitMax, "gave up after %v, before its bound", took)

	start = time.Now()
	waitPast(time.Time{})
	assert.Less(t, time.Since(start), clockWaitMax, "a zero stamp was waited for")
}

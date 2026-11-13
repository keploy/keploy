package relay

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/utils/stall"
)

// newTestTeeClock is newTestTeeWithConsumer on an injected stall clock, with a
// consumer that never goes away.
func newTestTeeClock(t *testing.T, capBytes int64, buf int, clk stall.Clock) (*tee, *dropRecorder) {
	t.Helper()
	rec := &dropRecorder{}
	t2 := newTee(fakeconn.FromClient, capBytes, buf, testStallGrace, rec.record, nil)
	t2.clock = clk
	t2.start(make(chan struct{}))
	t.Cleanup(func() {
		t2.close()
		t2.waitDone()
	})
	return t2, rec
}

// newClosedTestTeeClock is newTestTeeClock for a tee closed with chunks queued
// before its drain starts: the drain first waits on its consumer after
// close(), so the stall bound's verdict is all that measures the stall (a
// drain that waits while the tee is open looks at the stall too:
// lookAtStall), and the looks the clock is asked for are the verdict's alone.
func newClosedTestTeeClock(t *testing.T, capBytes int64, buf int, clk stall.Clock, chunks ...fakeconn.Chunk) (*tee, *dropRecorder) {
	t.Helper()
	rec := &dropRecorder{}
	t2 := newTee(fakeconn.FromClient, capBytes, buf, testStallGrace, rec.record, nil)
	t2.clock = clk
	for _, c := range chunks {
		t2.push(c)
	}
	t2.close()
	t2.start(make(chan struct{}))
	t.Cleanup(t2.waitDone)
	return t2, rec
}

// A consumer that stopped reading is abandoned after one window of run time,
// on a process held off the CPU as on one that is not: at a tenth of a CPU,
// ten windows of wall time.
func TestTee_StalledConsumerIsAbandonedAfterAWindowOfRunTime(t *testing.T) {
	clk := &stall.ManualClock{Step: func(d time.Duration) time.Duration { return d / 10 }} // a tenth of a CPU
	chunks := make([]fakeconn.Chunk, 5)
	for i := range chunks {
		chunks[i] = mkChunk("x")
	}
	tt, rec := newClosedTestTeeClock(t, 1<<30, 1, clk, chunks...)
	tt.waitDone()
	if n := rec.count(DropConsumerGone); n != 1 {
		t.Fatalf("consumer_gone reported %d times, want 1", n)
	}
	check := stall.CheckEvery(testStallGrace)
	if got, want := clk.Waits, int(testStallGrace/(check/10)); got != want {
		t.Fatalf("abandoned after %d looks, want %d: %v of run time at a tenth of a CPU", got, want, testStallGrace)
	}
}

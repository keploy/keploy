package relay

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/utils/stall"
)

// A closed tee's drain whose stall verdict cannot come still ends at an
// event: its consumer's exit (the parser closes its FakeConn, as the agent's
// stop does to every session). keploy's runnable clock is frozen here, as an
// overloaded host all but freezes it; the consumer never reads. The drain
// holds the queue while the consumer is there, the verdict pending, and gives
// it up once, as gone, at the exit: every chunk is delivered or counted.
func TestTee_ClosedDrainEndsAtItsConsumersExitWhileItsVerdictIsPending(t *testing.T) {
	t.Parallel()
	const queued = 50
	rec := &dropRecorder{}
	tt := newTee(fakeconn.FromClient, 1<<30, 1, testStallGrace, rec.record, nil)
	tt.clock = &stall.ManualClock{} // frozen: it never moves
	gone := make(chan struct{})
	tt.start(gone)
	for i := 0; i < queued; i++ {
		tt.push(mkChunk(fmt.Sprintf("c%03d", i)))
	}
	tt.close()
	ended := make(chan struct{})
	go func() { tt.waitDone(); close(ended) }()
	select {
	case <-ended:
		t.Fatal("the drain ended while its consumer was there and its stall verdict could not come")
	case <-time.After(2 * testStallGrace):
	}
	close(gone)
	select {
	case <-ended:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at its consumer's exit, its stall verdict pending")
	}
	if n := rec.count(DropConsumerGone); n != 1 {
		t.Errorf("consumer_gone reported %d times, want once for the connection", n)
	}
	delivered := 0
	for range tt.readCh() {
		delivered++
	}
	if got := delivered + int(tt.dropCount()); got != queued {
		t.Errorf("%d delivered + %d dropped = %d, want %d", delivered, tt.dropCount(), got, queued)
	}
}

// waitTeeDone waits for tt's drain to return. A drain that misses its
// consumer's exit never returns: the bound only turns that hang into a
// failure.
func waitTeeDone(t *testing.T, tt *tee) {
	t.Helper()
	ended := make(chan struct{})
	go func() { tt.waitDone(); close(ended) }()
	select {
	case <-ended:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at its consumer's exit")
	}
}

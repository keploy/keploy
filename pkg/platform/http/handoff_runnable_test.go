package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
)

// A decoded mock waits for a consumer that is slow while its stream is live,
// however long that takes: a slow consumer is back-pressure. Before, a
// consumer that took nothing for the grace (a mock file whose lock a whole
// file rewrite held for 49 s) had the mock dropped and the mock stream ended
// for the rest of the recording, while its test cases went on being saved
// without mocks.
func TestAMockWaitsForASlowConsumerWhileItsStreamIsLive(t *testing.T) {
	const grace = 50 * time.Millisecond
	clk := &stall.ManualClock{}
	ch := make(chan int)
	taken := make(chan bool, 1)
	go func() { taken <- handOff(context.Background(), ch, 1, clk, grace) }()
	// Many graces of time the process could run, and seconds of wall time:
	// no bound of either kind gives the mock up.
	for i := 0; i < 6; i++ {
		clk.Advance(grace)
		time.Sleep(500 * time.Millisecond)
	}
	select {
	case ok := <-taken:
		t.Fatalf("a live stream's mock was given up on (taken=%v) while its consumer was slow", ok)
	default:
	}
	if got := <-ch; got != 1 {
		t.Fatalf("the consumer took %d", got)
	}
	if !<-taken {
		t.Fatal("a mock the consumer took was said to be dropped")
	}
}

// Once the stream's client has ended it, the mock in hand is offered for the
// grace of the time the process could run: the recording's drain reads on what
// was queued after it ends the stream, also when it is starved of CPU. A
// consumer that took nothing for the grace of run time is given up on, so a
// stream no one reads any more does not hang.
func TestAMockIsOfferedForTheGraceOnceItsStreamEnded(t *testing.T) {
	const grace = 50 * time.Millisecond
	clk := &stall.ManualClock{}
	ch := make(chan int)
	ended, end := context.WithCancel(context.Background())
	end()
	taken := make(chan bool, 1)
	go func() { taken <- handOff(ended, ch, 1, clk, grace) }()
	// Four graces of real wall time, no CPU: neither the clock's wall time
	// (its timers) nor the host's may count.
	for stop := time.Now().Add(4 * grace); time.Now().Before(stop); {
		waitLook(t, clk)
		clk.Pass()
	}
	waitLook(t, clk)
	select {
	case <-taken:
		t.Fatal("a mock was given up on for wall time the consumer could not run")
	default:
	}
	if got := <-ch; got != 1 {
		t.Fatalf("the consumer took %d", got)
	}
	if !<-taken {
		t.Fatal("a mock the consumer took was said to be dropped")
	}

	go func() { taken <- handOff(ended, ch, 2, clk, grace) }()
	waitLook(t, clk)
	clk.Advance(grace)
	select {
	case ok := <-taken:
		if ok {
			t.Fatal("a mock no consumer took was said to be handed off")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a consumer that took nothing for the grace of run time after its stream ended was not given up on")
	}
}

// waitLook waits for a hand-off to arm its next look on clk.
func waitLook(t *testing.T, clk *stall.ManualClock) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the hand-off never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
}

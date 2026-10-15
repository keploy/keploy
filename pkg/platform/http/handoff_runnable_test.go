package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
)

// A decoded mock is handed to a consumer starved of CPU when it gets to run:
// the grace is time the process could run. Before, it was 30 s of wall time,
// and a CLI that waited longer for the CPU dropped the mock (the recording
// went on without it). A consumer that took nothing for the grace of run time
// is given up on.
func TestAMockWaitsForAConsumerStarvedOfCPU(t *testing.T) {
	const grace = 50 * time.Millisecond
	clk := &stall.ManualClock{}
	ch := make(chan int)
	taken := make(chan bool, 1)
	go func() { taken <- handOff(ch, 1, clk, grace) }()
	// Four graces of real wall time, no CPU: neither the clock's wall time
	// (its timers) nor the host's may count.
	for end := time.Now().Add(4 * grace); time.Now().Before(end); {
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

	go func() { taken <- handOff(ch, 2, clk, grace) }()
	waitLook(t, clk)
	clk.Advance(grace)
	select {
	case ok := <-taken:
		if ok {
			t.Fatal("a mock no consumer took was said to be handed off")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a consumer that took nothing for the grace of run time was not given up on")
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

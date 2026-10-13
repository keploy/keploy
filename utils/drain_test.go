package utils

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TestDrainErrGroupReturnsError verifies the normal path: when the group's
// goroutines finish before the timeout, DrainErrGroup returns g.Wait()'s error
// verbatim (and does not wait out the timeout).
func TestDrainErrGroupReturnsError(t *testing.T) {
	var g errgroup.Group
	want := errors.New("boom")
	g.Go(func() error { return want })

	start := time.Now()
	got := DrainErrGroup(zap.NewNop(), "test", &g, time.Second)
	if !errors.Is(got, want) {
		t.Fatalf("DrainErrGroup err = %v, want %v", got, want)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("DrainErrGroup blocked for %s on a fast group; should return immediately", elapsed)
	}
}

// TestDrainErrGroupTimesOutOnHang is the regression guard for the shutdown
// deadlock: a goroutine that ignores cancellation (here, blocks forever) must
// NOT be able to make DrainErrGroup block past its timeout. Without the bound,
// the teardown that calls this would hang until an external SIGKILL.
func TestDrainErrGroupTimesOutOnHang(t *testing.T) {
	var g errgroup.Group
	block := make(chan struct{})
	g.Go(func() error {
		<-block // simulates a goroutine that never observes context cancellation
		return nil
	})

	start := time.Now()
	got := DrainErrGroup(zap.NewNop(), "test", &g, 50*time.Millisecond)
	elapsed := time.Since(start)

	if got != nil {
		t.Fatalf("DrainErrGroup err = %v, want nil on timeout", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DrainErrGroup did not return promptly on a hung group: took %s (the deadlock would persist)", elapsed)
	}
	close(block) // unblock the leaked goroutine so the test process stays clean
}

// TestDrainErrGroupStatusCleanNotTimedOut verifies the signal the record
// teardown gates its post-record hook on: when every goroutine returns before
// the timeout, timedOut is false even when g.Wait() surfaces an error (the group
// still finished — nothing is left writing).
func TestDrainErrGroupStatusCleanNotTimedOut(t *testing.T) {
	var g errgroup.Group
	want := errors.New("boom")
	g.Go(func() error { return want })

	got, timedOut := DrainErrGroupStatus(zap.NewNop(), "test", &g, time.Second)
	if !errors.Is(got, want) {
		t.Fatalf("DrainErrGroupStatus err = %v, want %v", got, want)
	}
	if timedOut {
		t.Fatal("DrainErrGroupStatus timedOut = true on a group that finished; the post-record hook would be wrongly skipped")
	}
}

// TestDrainErrGroupStatusTimeoutReportsTimedOut is the guard for the B1 fix: a
// goroutine that ignores cancellation must make timedOut true (so the caller
// knows a writer may still be in flight) while err stays nil (a timeout never
// fails teardown).
func TestDrainErrGroupStatusTimeoutReportsTimedOut(t *testing.T) {
	var g errgroup.Group
	block := make(chan struct{})
	g.Go(func() error {
		<-block // never observes cancellation
		return nil
	})

	got, timedOut := DrainErrGroupStatus(zap.NewNop(), "test", &g, 50*time.Millisecond)
	if got != nil {
		t.Fatalf("DrainErrGroupStatus err = %v, want nil on timeout", got)
	}
	if !timedOut {
		t.Fatal("DrainErrGroupStatus timedOut = false on a hung group; the post-record hook would run while a mock write is still in flight")
	}
	close(block) // unblock the leaked goroutine so the test process stays clean
}

// A drain whose bound has passed does not give up while an agent stop is in
// progress (BeginAgentStop): the stop ends on its own -- at the agent's exit,
// at a kill once its teardown stops making progress, or at a second Ctrl+C --
// and a drain that gave up first let keploy exit with the agent still running.
func TestADrainWaitsForAnAgentStopInProgress(t *testing.T) {
	for _, progressDrain := range []bool{false, true} {
		end := BeginAgentStop()
		var g errgroup.Group
		release := make(chan struct{})
		g.Go(func() error { <-release; return nil }) // the stop goroutine
		returned := make(chan bool, 1)
		go func() {
			var timedOut bool
			if progressDrain {
				_, timedOut = DrainErrGroupProgress(zap.NewNop(), "setup", &g, time.Millisecond, func() uint64 { return 0 })
			} else {
				_, timedOut = DrainErrGroupStatus(zap.NewNop(), "setup", &g, time.Millisecond)
			}
			returned <- timedOut
		}()
		select {
		case <-returned:
			t.Fatalf("progress=%v: the drain gave up while an agent stop was in progress", progressDrain)
		case <-time.After(100 * time.Millisecond): // only that it has NOT returned yet
		}
		close(release) // the stop's goroutine returns as the stop ends
		end()
		select {
		case timedOut := <-returned:
			if timedOut {
				t.Fatalf("progress=%v: the drain gave up although the group joined once the stop ended", progressDrain)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("progress=%v: the drain never returned", progressDrain)
		}
	}
	// With no stop in progress the bound holds as before.
	var g errgroup.Group
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	g.Go(func() error { <-block; return nil })
	if _, timedOut := DrainErrGroupStatus(zap.NewNop(), "setup", &g, time.Millisecond); !timedOut {
		t.Fatal("a drain with no agent stop in progress did not give up at its bound")
	}
}

// AwaitAgentExit waits for an agent's exit as a drain waits for its group: it
// gives up after its bound -- unless an agent stop that will end on its own
// holds the drains, when it waits for that and gives the exit the bound again
// -- and at an interrupt given while it waits.
func TestAwaitAgentExit(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	if !AwaitAgentExit(closed, time.Hour, nil) {
		t.Fatal("an agent that had exited was not waited for")
	}
	never := make(chan struct{})
	if AwaitAgentExit(never, time.Millisecond, nil) {
		t.Fatal("an agent that never exits was reported gone")
	}
	if AwaitAgentExit(never, time.Hour, closed) {
		t.Fatal("an interrupt did not end the wait")
	}

	end := BeginAgentStop()
	gone := make(chan struct{})
	returned := make(chan bool, 1)
	go func() { returned <- AwaitAgentExit(gone, time.Millisecond, nil) }()
	select {
	case <-returned:
		t.Fatal("gave up on the exit while an agent stop held the drains")
	case <-time.After(100 * time.Millisecond): // only that it has NOT returned yet
	}
	close(gone) // the stop ends as the agent exits
	end()
	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("the exit that came as the stop ended was not seen")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("never returned")
	}
}

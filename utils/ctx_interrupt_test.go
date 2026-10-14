package utils

import (
	"testing"
	"time"
)

// The CLI cancels its own root context when a `keploy mock` run ends -- that
// is how its goroutines are torn down -- so a cancelled context says nothing
// about whether a signal arrived. A caller that read the two as the same thing
// reported every recording the VS Code extension made as "interrupted", and
// the panel told the user their recording had been cut short over one that had
// just succeeded.
func TestInterruptedIsNotSetByTheCLIsOwnTeardown(t *testing.T) {
	t.Cleanup(ClearInterrupted)
	ClearInterrupted()

	ctx := NewCtx()
	if Interrupted() {
		t.Fatal("a fresh context reports an interrupt that never happened")
	}
	// What cli/mock.go does from its defer at the end of every run.
	ExecCancel()
	if ctx.Err() == nil {
		t.Fatal("ExecCancel did not cancel the root context")
	}
	if Interrupted() {
		t.Fatal("the CLI's own teardown was reported as a signal")
	}

	// A real signal does set it.
	MarkInterrupted()
	if !Interrupted() {
		t.Fatal("a signal was not reported")
	}
}

// InterruptedAfter hears only the interrupts that come after the count a stop
// took as it began: a stop delivered twice before it (`sudo pkill -f`) is no
// interrupt of it, and a later one is.
func TestInterruptedAfterHearsOnlyLaterInterrupts(t *testing.T) {
	ResetInterruptedAgain()
	t.Cleanup(ResetInterruptedAgain)
	heard := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	MarkInterruptedAgain() // the first stop, delivered twice
	since := RepeatInterrupts()
	if since != 1 {
		t.Fatalf("RepeatInterrupts() = %d, want 1", since)
	}
	again := InterruptedAfter(since)
	if heard(again) {
		t.Fatal("an interrupt from before the stop was heard as one during it")
	}
	if !heard(InterruptedAfter(since - 1)) {
		t.Fatal("an interrupt after an earlier count was not heard")
	}
	MarkInterruptedAgain() // during the stop
	if !heard(again) {
		t.Fatal("an interrupt during the stop was not heard")
	}
}

// A stop offers to end at the user's next interrupt only once its first signal
// has come, and a whole notice period, as measured, after that signal was
// first seen (StopNowNotice): a single stop can arrive twice (sudo hands its
// own copy on when a signal reaches both it and keploy), and the copy comes
// with the first, never a period later. Whether the stop began with the
// signal, or on its own (the app exited, a record timer) with the signal
// coming during its wait, and even when a notice is read late, the copy ends
// nothing, and the user's interrupt after the offer ends the stop. A withdrawn
// offer ends nothing.
func TestStopNowNoticeOffersOnlyAfterACopyOfTheStopHasCome(t *testing.T) {
	reset := func() { ResetInterruptedAgain(); ClearInterrupted() }
	t.Cleanup(reset)
	now := time.Unix(1_000_000, 0)
	defer SetNoticeClockForTest(func() time.Time { return now })()
	const period = 5 * time.Second
	stopped := func() bool {
		select {
		case <-StopNow():
			return true
		default:
			return false
		}
	}

	t.Run("an offer before any signal is none", func(t *testing.T) {
		reset()
		OfferStopNow()
		MarkInterrupted()
		if MarkInterruptedAgain() || stopped() {
			t.Fatal("an offer made before the stop's signal let its copy end the stop")
		}
	})

	t.Run("a stop begun by a signal", func(t *testing.T) {
		reset()
		MarkInterrupted() // the stop
		n := NewStopNowNotice(period)
		if MarkInterruptedAgain() || stopped() {
			t.Fatal("the stop's copy ended it before it said anything")
		}
		now = now.Add(period - time.Nanosecond)
		if n.Next() {
			t.Fatal("a notice offered before a whole period after the signal")
		}
		now = now.Add(time.Nanosecond)
		if !n.Next() {
			t.Fatal("the notice a whole period after the stop's signal does not offer")
		}
		if !MarkInterruptedAgain() || !stopped() {
			t.Fatal("the user's interrupt after the offer did not end the stop")
		}
		n.Withdraw()
	})

	t.Run("a stop begun on its own, then interrupted, its notices read late", func(t *testing.T) {
		reset()
		n := NewStopNowNotice(period)
		now = now.Add(period)
		if n.Next() {
			t.Fatal("a notice offered before any signal")
		}
		MarkInterrupted() // the user's stop, or a pattern kill's first copy
		now = now.Add(period)
		if n.Next() {
			t.Fatal("the notice that first found the signal offered at once: its copy could still come")
		}
		if MarkInterruptedAgain() || stopped() { // the copy, after that notice
			t.Fatal("the copy of the signal ended the stop")
		}
		now = now.Add(period / 2) // a notice read late, as its loop was busy
		if n.Next() {
			t.Fatal("a notice read late offered less than a whole period after the signal was seen")
		}
		now = now.Add(period / 2)
		if !n.Next() {
			t.Fatal("the notice a whole period after the signal was seen does not offer")
		}
		if !MarkInterruptedAgain() || !stopped() {
			t.Fatal("the user's interrupt after the offer did not end the stop")
		}
		n.Withdraw()
	})

	t.Run("a withdrawn offer ends nothing", func(t *testing.T) {
		reset()
		MarkInterrupted()
		n := NewStopNowNotice(period)
		now = now.Add(period)
		if !n.Next() {
			t.Fatal("setup: no offer")
		}
		n.Withdraw()
		n.Withdraw() // once only
		if MarkInterruptedAgain() || stopped() {
			t.Fatal("an interrupt after the stop withdrew its offer ended it")
		}
		if RepeatInterrupts() != 1 {
			t.Fatalf("RepeatInterrupts() = %d, want every repeat counted", RepeatInterrupts())
		}
	})
}

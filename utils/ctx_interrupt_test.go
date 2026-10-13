package utils

import "testing"

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

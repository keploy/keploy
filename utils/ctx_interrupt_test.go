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

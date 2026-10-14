package replay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// setupInstr is an agent whose Setup is the test's.
type setupInstr struct {
	*bpNoAgent
	setup func(ctx context.Context) error
}

func (s *setupInstr) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	return s.setup(ctx)
}

// A replay its user cancels (Ctrl-C) while keploy sets up the environment ends
// as an interrupted run, as record does: its error is the run's cancellation,
// no ERROR line is logged, and the exit code is 0 (as it was). The setup's
// error kept that cause; Instrument replaced it with its own words
// (keploy#4786), so Start logged "failed to instrument" at ERROR. A setup that fails on its own
// is logged at ERROR by Start with its cause, and fails the run.
func TestASetupTheRunCancelledIsTheRunsCancellation(t *testing.T) {
	run := newBasePathRun(t, "", "t1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run.replayer.instrument = true
	run.replayer.instrumentation = &setupInstr{bpNoAgent: run.agent, setup: func(ctx context.Context) error {
		cancel() // the user's Ctrl-C, while the setup waits on the daemon
		<-ctx.Done()
		return ctx.Err()
	}}
	code, err := run.startIn(t, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v, want the run's cancellation", err)
	}
	if logged := run.errorLogs(); len(logged) != 0 {
		t.Fatalf("a cancelled setup logged at ERROR: %q", logged)
	}
	if code != 0 {
		t.Fatalf("exit code %d, want 0 for a run its user stopped", code)
	}

	run = newBasePathRun(t, "", "t1")
	boom := errors.New("the agent did not start")
	run.replayer.instrument = true
	run.replayer.instrumentation = &setupInstr{bpNoAgent: run.agent, setup: func(context.Context) error { return boom }}
	code, err = run.startIn(t, context.Background())
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v, want the setup's failure", err)
	}
	// Start's own line, naming the cause (Instrument logs the failure too).
	logged := run.errorLogs()
	if want := "failed to instrument: failed setting up the environment: " + boom.Error(); !strings.Contains(strings.Join(logged, "\n"), want) {
		t.Fatalf("a failed setup did not log %q at ERROR: %q", want, logged)
	}
	// Start's error names the cause and stays flat: its callers unwrap
	// nothing new from a failure.
	if want := "failed to instrument: failed setting up the environment: " + boom.Error(); err.Error() != want || errors.Is(err, boom) {
		t.Fatalf("Start: %v, want the flat %q", err, want)
	}
	if code != 1 {
		t.Fatalf("exit code %d, want 1 for a failed setup", code)
	}
}

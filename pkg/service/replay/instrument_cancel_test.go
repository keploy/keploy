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
// no ERROR line is logged, and the exit code is 0. The setup's error kept
// that cause; keploy#4786 had Instrument replace it with its own words, so
// Start logged "failed to instrument" at ERROR. A setup that fails on its own
// is logged at ERROR with its cause, and fails the run.
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
	logged := run.errorLogs()
	if len(logged) == 0 || !strings.Contains(strings.Join(logged, "\n"), "failed setting up the environment") {
		t.Fatalf("a failed setup was not logged at ERROR: %q", logged)
	}
	if !strings.Contains(err.Error(), boom.Error()) {
		t.Fatalf("Start: %v, want the setup's own error in it", err)
	}
	if code != 1 {
		t.Fatalf("exit code %d, want 1 for a failed setup", code)
	}
}

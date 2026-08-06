package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// stubAgent satisfies agent.Service by embedding it: only Setup is ever called
// on this path, so the remaining methods stay nil and would panic loudly if the
// command ever started using them.
type stubAgent struct {
	agent.Service
	setupErr error
}

func (s stubAgent) Setup(context.Context, chan int) error { return s.setupErr }

type stubFactory struct {
	svc interface{}
	err error
}

func (f stubFactory) GetService(context.Context, string) (interface{}, error) {
	return f.svc, f.err
}

// stubConfigurator registers only the flags Agent's RunE reads back.
type stubConfigurator struct{}

func (stubConfigurator) AddFlags(cmd *cobra.Command) error {
	cmd.Flags().Uint32("client-pid", 0, "")
	// Kept true by the tests so the parent-death watchdog stays disabled;
	// it watches a real PID and has no place in a unit test.
	cmd.Flags().Bool("is-docker", true, "")
	return nil
}
func (stubConfigurator) ValidateFlags(context.Context, *cobra.Command) error { return nil }
func (stubConfigurator) Validate(context.Context, *cobra.Command) error      { return nil }

func runAgent(t *testing.T, f ServiceFactory) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := Agent(ctx, zap.NewNop(), &config.Config{}, f, stubConfigurator{})
	if cmd == nil {
		t.Fatal("Agent returned a nil command")
	}
	cmd.SetArgs([]string{"--is-docker=true"})
	cmd.SetOut(nopWriter{})
	cmd.SetErr(nopWriter{})
	return cmd.Execute()
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// An agent that cannot set up must report a FAILURE to its caller. Returning
// nil here makes keploy exit 0, so kubelet records a dead agent as
// "Reason: Completed, Exit Code: 0" and every orchestrator above it reads a
// hard failure as a clean shutdown.
func TestAgent_SetupFailurePropagates(t *testing.T) {
	want := errors.New("failed to hook into the app")

	err := runAgent(t, stubFactory{svc: stubAgent{setupErr: want}})

	if !errors.Is(err, want) {
		t.Fatalf("setup failure did not propagate: got %v, want %v", err, want)
	}
}

// A cancelled context is how the agent is asked to STOP (SIGINT/SIGTERM,
// parent exit). That is a normal shutdown and must keep exiting 0, or every
// clean stop would be reported as a crash.
func TestAgent_GracefulCancellationIsNotAFailure(t *testing.T) {
	err := runAgent(t, stubFactory{svc: stubAgent{setupErr: context.Canceled}})

	if err != nil {
		t.Fatalf("graceful cancellation reported as failure: %v", err)
	}
}

// Wrapped cancellations count as graceful too — Setup returns the context
// error through layers of its own wrapping.
func TestAgent_WrappedCancellationIsNotAFailure(t *testing.T) {
	err := runAgent(t, stubFactory{svc: stubAgent{
		setupErr: errors.Join(errors.New("shutting down proxy"), context.Canceled),
	}})

	if err != nil {
		t.Fatalf("wrapped cancellation reported as failure: %v", err)
	}
}

func TestAgent_ServiceLookupFailurePropagates(t *testing.T) {
	want := errors.New("no service")

	err := runAgent(t, stubFactory{err: want})

	if !errors.Is(err, want) {
		t.Fatalf("service lookup failure did not propagate: got %v, want %v", err, want)
	}
}

// A service that does not implement agent.Service is a wiring bug; it must not
// exit 0 either.
func TestAgent_WrongServiceTypePropagates(t *testing.T) {
	err := runAgent(t, stubFactory{svc: struct{}{}})

	if err == nil {
		t.Fatal("wrong service type reported success")
	}
}

// A successful setup must stay quiet.
func TestAgent_SuccessReturnsNil(t *testing.T) {
	if err := runAgent(t, stubFactory{svc: stubAgent{}}); err != nil {
		t.Fatalf("successful setup returned %v", err)
	}
}

// The real shutdown shape: agent.Hook wraps whatever error the interrupted
// step produced, so a stop that races startup need NOT carry context.Canceled.
// The command owns the context, so its state is the authoritative answer to
// "was this a shutdown?" — without that check a deleted pod would report a
// crash and get restarted.
func TestAgent_CancelledContextIsNotAFailureEvenWithAnUnrelatedError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already requested before Setup returns

	cmd := Agent(ctx, zap.NewNop(), &config.Config{},
		stubFactory{svc: stubAgent{setupErr: errors.New("failed to hook into the app")}},
		stubConfigurator{})
	cmd.SetArgs([]string{"--is-docker=true"})
	cmd.SetOut(nopWriter{})
	cmd.SetErr(nopWriter{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("shutdown during startup reported as failure: %v", err)
	}
}

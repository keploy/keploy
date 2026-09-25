package cli

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func TestIsDaemonSetAgent(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{name: "empty (equivalent to unset)", val: "", want: false},
		{name: "true", val: "true", want: true},
		// Only the exact string "true" gates DaemonSet mode, matching k8s-proxy's
		// canonical daemonsetenv package and pkg/agent/hooks/linux/hooks.go — "1"
		// must NOT be accepted, or this gate would diverge from the rest of the
		// system on a KEPLOY_DAEMONSET_ENABLED=1 pod.
		{name: "one is not accepted", val: "1", want: false},
		{name: "false", val: "false", want: false},
		{name: "zero", val: "0", want: false},
		{name: "uppercase TRUE is not accepted", val: "TRUE", want: false},
		{name: "arbitrary", val: "yes", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv restores the previous value after the test; an empty value
			// is treated the same as unset by isDaemonSetAgent.
			t.Setenv("KEPLOY_DAEMONSET_ENABLED", tc.val)
			if got := isDaemonSetAgent(); got != tc.want {
				t.Errorf("isDaemonSetAgent() with KEPLOY_DAEMONSET_ENABLED=%q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// agentSvc is an agent service whose Setup fails, or ends, the way it is told.
// Every other method panics: RunE must not reach them.
type agentSvc struct {
	agent.Service
	setup func(ctx context.Context) error
}

func (s agentSvc) Setup(ctx context.Context, _ chan int) error { return s.setup(ctx) }

type agentSvcFactory struct {
	svc interface{}
	err error
}

func (f agentSvcFactory) GetService(context.Context, string) (interface{}, error) {
	return f.svc, f.err
}

type noFlags struct{}

func (noFlags) AddFlags(*cobra.Command) error                       { return nil }
func (noFlags) ValidateFlags(context.Context, *cobra.Command) error { return nil }
func (noFlags) Validate(context.Context, *cobra.Command) error      { return nil }

// runAgent runs the agent command's RunE against factory and returns the exit
// code the process would end with.
func runAgent(t *testing.T, ctx context.Context, factory ServiceFactory) int {
	t.Helper()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	cmd := Agent(ctx, zap.NewNop(), &config.Config{}, factory, noFlags{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned %v; the agent reports failure through its exit code", err)
	}
	return utils.ErrCode
}

// An agent that could not start has to exit non-zero, and with the specific
// code its failure carries: that status is all the launching CLI (and docker,
// and the kubelet) ever sees of it. It exited 0, so a native `keploy mock
// record` whose agent had died reported success without running the tests.
func TestAgentThatCannotStartExitsNonZero(t *testing.T) {
	failing := func(err error) ServiceFactory {
		return agentSvcFactory{svc: agentSvc{setup: func(context.Context) error { return err }}}
	}
	for _, tc := range []struct {
		name    string
		factory ServiceFactory
		want    int
	}{
		{"privileges refused", failing(fmt.Errorf("failed to hook into the app: %w: failed to set memlock rlimit: %w", utils.ErrPrivilegeRequired, syscall.EPERM)), utils.ExitPrivilegeRequired},
		{"environment lacks tracefs", failing(fmt.Errorf("failed to hook into the app: %w: neither debugfs nor tracefs are mounted", utils.ErrEnvironmentUnsupported)), utils.ExitEnvironmentUnsupported},
		{"any other failure", failing(errors.New("failed to hook into the app: address already in use")), utils.ExitKeployError},
		{"no service", agentSvcFactory{err: errors.New("no such service")}, utils.ExitKeployError},
		{"not an agent service", agentSvcFactory{svc: struct{}{}}, utils.ExitKeployError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if got := runAgent(t, ctx, tc.factory); got != tc.want {
				t.Fatalf("exit code %d, want %d", got, tc.want)
			}
		})
	}
}

// The ways a HEALTHY agent ends must stay exit 0, or every clean stop reads as
// a crash: Setup returns context.Canceled when a serving agent is stopped, nil
// when it is stopped before it announced its port, and an error that arrives
// after the process was told to stop is teardown.
func TestAgentThatIsStoppedExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(ctx context.Context, stop context.CancelFunc) error
	}{
		{"stopped while serving", func(context.Context, context.CancelFunc) error { return context.Canceled }},
		{"stopped before announcing its port", func(context.Context, context.CancelFunc) error { return nil }},
		{"teardown error after the stop", func(_ context.Context, stop context.CancelFunc) error {
			stop()
			return errors.New("error during agent setup: proxy closed")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			factory := agentSvcFactory{svc: agentSvc{setup: func(ctx context.Context) error { return tc.setup(ctx, cancel) }}}
			if got := runAgent(t, ctx, factory); got != 0 {
				t.Fatalf("a stopped agent exits %d", got)
			}
		})
	}
}

// A signal is a stop for the agent too, not a failure: it keeps the one rule
// every command keeps (utils.SetFailureExitCode). That covers the window in
// which the signal has been marked but nothing is cancelled yet -- the drain
// utils.NewCtx's handler holds a sidecar agent in, serving, before it cancels
// (KEPLOY_SIDECAR_DRAIN_SECONDS) -- which agentFailed, reading the context,
// cannot see.
func TestAgentASignalStoppedExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory ServiceFactory
	}{
		{"failed while draining", agentSvcFactory{svc: agentSvc{setup: func(context.Context) error {
			return errors.New("failed to hook into the app: address already in use")
		}}}},
		{"no service", agentSvcFactory{err: errors.New("no such service")}},
		{"not an agent service", agentSvcFactory{svc: struct{}{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ClearInterrupted()
			t.Cleanup(utils.ClearInterrupted)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			utils.MarkInterrupted()
			if got := runAgent(t, ctx, tc.factory); got != 0 {
				t.Fatalf("an agent a signal stopped exits %d", got)
			}
		})
	}
}

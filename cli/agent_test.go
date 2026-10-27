package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

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

// agentHangupHelperEnv, set, makes TestAgentDiesOfAHangUp the agent the
// hang-up tests start: a copy of this test binary that builds the agent
// command on NewCtx's context, runs its PreRunE, marks "ready" and waits for
// that context to end. With agentHangupHoldEnv set too, its shutdown then
// marks "stopping" and lasts until the test writes "release".
const (
	agentHangupHelperEnv = "KEPLOY_TEST_AGENT_HANGUP"
	agentHangupHoldEnv   = "KEPLOY_TEST_AGENT_HANGUP_HOLD"
)

// startHangupAgent starts that agent and returns once it is ready.
func startHangupAgent(t *testing.T, hold bool) (dir string, agent *exec.Cmd, out *bytes.Buffer, exited chan error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGHUP")
	}
	// Go starts a child with the default action for a signal it catches, so
	// the agent starts with SIGHUP's default even when `go test` runs under
	// nohup.
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(caught) })

	dir = t.TempDir()
	agent = exec.Command(os.Args[0], "-test.run=^TestAgentDiesOfAHangUp$")
	agent.Env = append(os.Environ(), agentHangupHelperEnv+"="+dir)
	if hold {
		agent.Env = append(agent.Env, agentHangupHoldEnv+"=1")
	}
	out = &bytes.Buffer{}
	agent.Stdout, agent.Stderr = out, out
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	exited = make(chan error, 1)
	go func() { exited <- agent.Wait() }()
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		select {
		case <-exited:
		default:
		}
	})
	waitForMark(t, dir, "ready", exited, out)
	return dir, agent, out, exited
}

// waitForMark returns once the agent has written name in dir, and fails the
// test if the agent ends first.
func waitForMark(t *testing.T, dir, name string, exited chan error, out *bytes.Buffer) {
	t.Helper()
	deadline := time.After(30 * time.Second) // only turns a hang into a failure
	for {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("the agent ended before %q: %v\n%s", name, err, out.String())
		case <-deadline:
			t.Fatalf("the agent never got to %q\n%s", name, out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// awaitAgentEnd returns how the agent ended, failing the test if it runs on.
func awaitAgentEnd(t *testing.T, agent *exec.Cmd, exited chan error, out *bytes.Buffer) syscall.WaitStatus {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatalf("the agent was still running 30s after it was told to end\n%s", out.String())
	}
	return agent.ProcessState.Sys().(syscall.WaitStatus)
}

// `keploy agent` dies of a hang-up, as it did before NewCtx made one stop
// keploy gracefully, because its owners count on that (utils.DieOnHangup).
func TestAgentDiesOfAHangUp(t *testing.T) {
	if dir := os.Getenv(agentHangupHelperEnv); dir != "" {
		ctx := utils.NewCtx()
		cmd := Agent(ctx, zap.NewNop(), &config.Config{}, agentSvcFactory{}, noFlags{})
		if err := cmd.PreRunE(cmd, nil); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
			os.Exit(3)
		}
		<-ctx.Done()
		if os.Getenv(agentHangupHoldEnv) != "" {
			if err := os.WriteFile(filepath.Join(dir, "stopping"), nil, 0o600); err != nil {
				os.Exit(3)
			}
			for {
				if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		os.Exit(0)
	}
	_, agent, out, exited := startHangupAgent(t, false)
	if err := agent.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if st := awaitAgentEnd(t, agent, exited, out); !st.Signaled() || st.Signal() != syscall.SIGHUP {
		t.Fatalf("the agent ended with %v, want it dead of SIGHUP\n%s", agent.ProcessState, out.String())
	}
}

// Once `keploy agent` is shutting down, a hang-up no longer kills it: its
// shutdown removes the TLS uprobes it attached, and a hang-up that killed it
// part way leaked them (utils.IgnoreHangupOnceStopping, which the agent
// command's PreRunE sets up). The CLI that started it on a PTY can exit while
// it shuts down, and that exit hangs it up.
func TestAgentSurvivesAHangUpOnceStopping(t *testing.T) {
	dir, agent, out, exited := startHangupAgent(t, true)
	if err := agent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForMark(t, dir, "stopping", exited, out)
	if err := agent.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := awaitAgentEnd(t, agent, exited, out); st.Signaled() || st.ExitStatus() != 0 {
		t.Fatalf("the agent ended with %v during its shutdown, want exit 0 once it finished\n%s", agent.ProcessState, out.String())
	}
}
